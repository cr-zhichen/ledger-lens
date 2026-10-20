package qianji

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"ledger-lens/internal/fault"
)

// Tag and tag group IDs are opaque strings in the app protocol.
var tagIDPattern = regexp.MustCompile(`^[0-9A-Za-z_-]{1,64}$`)

func ValidTagID(value string) bool { return tagIDPattern.MatchString(value) }

// SubmitTag creates a tag. The app sends {"groupid","name","color"} through
// Gson, which drops unset fields; an empty groupID means the default group.
func (c *Client) SubmitTag(ctx context.Context, s Session, name, groupID, color string) (json.RawMessage, string, error) {
	if strings.TrimSpace(name) == "" || groupID != "" && !ValidTagID(groupID) {
		return nil, "", fault.Invalid("标签需要名称及有效的标签组 ID")
	}
	fields := map[string]string{"name": name}
	if groupID != "" {
		fields["groupid"] = groupID
	}
	if color != "" {
		fields["color"] = color
	}
	raw, _ := json.Marshal(fields)
	data, err := c.object(ctx, "/tag/submittag", s, url.Values{"v": {string(raw)}})
	if err != nil {
		return nil, "", err
	}
	var tag struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(data, &tag)
	var id string
	if json.Unmarshal(tag.ID, &id) != nil {
		id = strings.TrimSpace(string(tag.ID))
	}
	if !ValidTagID(id) {
		return nil, "", invalidResponse()
	}
	return data, id, nil
}

func (c *Client) ArchiveTag(ctx context.Context, s Session, id string) error {
	if !ValidTagID(id) {
		return fault.Invalid("标签 ID 无效，请查询 tags list")
	}
	_, err := c.post(ctx, "/tag/archivetag", s, url.Values{"did": {id}})
	return err
}
