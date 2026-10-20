package service

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
	"ledger-lens/internal/store"
)

type CreateTag struct {
	Name    string  `json:"name"`
	GroupID *string `json:"group_id,omitempty"`
	Color   *string `json:"color,omitempty"`
}

type ArchiveTag struct {
	ID string `json:"id"`
}

type TagResult struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	Confirmed bool            `json:"confirmed"`
	Tag       json.RawMessage `json:"tag,omitempty"`
	SyncedAt  string          `json:"synced_at,omitempty"`
}

type tagEntry struct {
	id, name, group string
	raw             map[string]any
}

var tagColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// CreateTag refuses an existing name, so a retried command never adds a
// second tag with the same name.
func (s *Service) CreateTag(ctx context.Context, input CreateTag) (TagResult, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" || utf8.RuneCountInString(name) > 50 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return TagResult{}, fault.Invalid("标签名称不能为空、不能含控制字符，且不超过 50 个字符")
	}
	group, color := "", ""
	if input.GroupID != nil {
		group = *input.GroupID
		if !qianji.ValidTagID(group) {
			return TagResult{}, fault.Invalid("标签组 ID 无效，请查询 tags list")
		}
	}
	if input.Color != nil {
		color = *input.Color
		if !tagColor.MatchString(color) {
			return TagResult{}, fault.Invalid("color 必须是 #RRGGBB 格式，例如 #529af8")
		}
	}
	lock, err := s.Store.LockWrites()
	if err != nil {
		return TagResult{}, err
	}
	defer lock.Close()
	tags, groups, _, err := s.loadTags(ctx, -1)
	if err != nil {
		return TagResult{}, err
	}
	if group != "" && !groups[group] {
		return TagResult{}, fault.Invalid("标签组不存在，请查询 tags list")
	}
	for _, tag := range tags {
		if tag.name == name {
			err := fault.New("TAG_EXISTS", "已有同名标签，请直接使用该标签 ID", 2)
			err.Details = map[string]string{"id": tag.id}
			return TagResult{}, err
		}
	}
	client, err := s.writeClient(ctx)
	if err != nil {
		return TagResult{}, err
	}
	result := TagResult{Status: "unknown"}
	raw, id, err := client.SubmitTag(ctx, s.Account.Session, name, group, color)
	if err != nil {
		if definiteFailure(err) {
			return result, err
		}
		return tagFailure(result, "WRITE_UNCERTAIN", "标签创建结果无法确认，请执行 tags list 按名称核对，不要直接重试")
	}
	result.ID, result.Status, result.Tag = id, "applied", raw
	tags, _, catalog, err := s.loadTags(ctx, -1)
	if err != nil {
		return tagFailure(result, "WRITE_CONFIRMATION_FAILED", "钱迹已接受新建标签，但无法读取标签列表，请执行 tags list 核对，不要重复创建")
	}
	result.SyncedAt = catalog.SyncedAt
	for _, tag := range tags {
		if tag.id == id && tag.name == name {
			result.Confirmed = true
			result.Tag, _ = json.Marshal(tag.raw)
		}
	}
	if !result.Confirmed {
		return tagFailure(result, "WRITE_CONFIRMATION_FAILED", "钱迹已接受新建标签，但标签列表中尚未出现，请稍后执行 tags list 核对，不要重复创建")
	}
	return result, nil
}

// ArchiveTag only archives a tag that is currently active: the upstream route
// is not documented as idempotent and may toggle an archived tag back.
func (s *Service) ArchiveTag(ctx context.Context, input ArchiveTag) (TagResult, error) {
	if !qianji.ValidTagID(input.ID) {
		return TagResult{}, fault.Invalid("标签 ID 无效，请查询 tags list")
	}
	lock, err := s.Store.LockWrites()
	if err != nil {
		return TagResult{}, err
	}
	defer lock.Close()
	active, _, _, err := s.loadTags(ctx, 1)
	if err != nil {
		return TagResult{}, err
	}
	if !hasTag(active, input.ID) {
		return TagResult{}, fault.New("TAG_NOT_ACTIVE", "标签不存在或已归档，请查询 tags list --status 1", 2)
	}
	client, err := s.writeClient(ctx)
	if err != nil {
		return TagResult{}, err
	}
	result := TagResult{ID: input.ID, Status: "unknown"}
	// The response body carries no tag state; the archived list decides.
	if err := client.ArchiveTag(ctx, s.Account.Session, input.ID); err != nil && definiteFailure(err) {
		return result, err
	}
	archived, _, _, archivedErr := s.loadTags(ctx, 2)
	active, _, _, activeErr := s.loadTags(ctx, 1)
	if archivedErr != nil || activeErr != nil {
		return tagFailure(result, "WRITE_UNCERTAIN", "标签归档结果无法确认，请执行 tags list --status 2 核对，不要直接重试")
	}
	if !hasTag(archived, input.ID) || hasTag(active, input.ID) {
		return tagFailure(result, "WRITE_UNCERTAIN", "标签尚未出现在归档列表中，请执行 tags list --status 2 核对，不要直接重试")
	}
	result.Status, result.Confirmed = "applied", true
	for _, tag := range archived {
		if tag.id == input.ID {
			result.Tag, _ = json.Marshal(tag.raw)
		}
	}
	// Refresh the full tag directory used by bill validation.
	_, _, catalog, err := s.loadTags(ctx, -1)
	if err != nil {
		return tagFailure(result, "WRITE_CONFIRMATION_FAILED", "标签已归档，但本地标签缓存未刷新，请执行 cache refresh")
	}
	result.SyncedAt = catalog.SyncedAt
	return result, nil
}

// loadTags reads the full directory online and filters by each tag's own
// status: upstream tag/list ignores its status parameter.
func (s *Service) loadTags(ctx context.Context, status int) ([]tagEntry, map[string]bool, store.Catalog, error) {
	catalog, err := s.Catalog(ctx, "tags", false, func(c *qianji.Client, a qianji.Session) (json.RawMessage, error) { return c.Tags(ctx, a, -1, 0) })
	if err != nil {
		return nil, nil, catalog, err
	}
	groups, err := catalogItems(catalog.Data)
	if err != nil {
		return nil, nil, catalog, err
	}
	tags := []tagEntry{}
	ids := map[string]bool{}
	for _, group := range groups {
		groupID := textValue(group["id"])
		if groupID != "" {
			ids[groupID] = true
		}
		if group["tags"] == nil {
			continue
		}
		values, ok := group["tags"].([]any)
		if !ok {
			return nil, nil, catalog, invalidWriteData()
		}
		for _, value := range values {
			item, ok := value.(map[string]any)
			if !ok {
				return nil, nil, catalog, invalidWriteData()
			}
			if status != -1 && textValue(item["status"]) != strconv.Itoa(status) {
				continue
			}
			tags = append(tags, tagEntry{id: textValue(item["id"]), name: strings.TrimSpace(textValue(item["name"])), group: groupID, raw: item})
		}
	}
	return tags, ids, catalog, nil
}

func hasTag(tags []tagEntry, id string) bool {
	for _, tag := range tags {
		if tag.id == id {
			return true
		}
	}
	return false
}

// writeClient sends one request with the current session. Writes are never
// wrapped by Read's automatic re-authentication retry.
func (s *Service) writeClient(ctx context.Context) (*qianji.Client, error) {
	account, err := s.Store.Account(ctx)
	if err != nil {
		return nil, err
	}
	if account.Revision != s.Account.Revision {
		return nil, fault.New("STATE_CHANGED", "登录状态已改变，请核对账号后重新执行", 1)
	}
	return s.client(ctx, s.Account.BaseURL)
}

// definiteFailure reports upstream rejections that prove nothing was written.
func definiteFailure(err error) bool {
	for _, code := range []string{"TOKEN_EXPIRED", "AUTH_REQUIRED", "BUSINESS_ERROR", "SIGNATURE_REJECTED", "INVALID_ARGUMENT", "VIP_REQUIRED"} {
		if fault.Is(err, code) {
			return true
		}
	}
	return false
}

func tagFailure(result TagResult, code, message string) (TagResult, error) {
	err := fault.New(code, message, 4)
	details := result
	details.Tag = nil
	err.Details = details
	return result, err
}
