package qianji

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
)

func (c *Client) Refund(ctx context.Context, a Session, source string, payload map[string]any) ([]json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	data, err := c.post(ctx, "/bill/refund2", a, url.Values{"did": {source}, "v": {string(raw)}})
	if err != nil {
		return nil, err
	}
	var result struct {
		List []json.RawMessage `json:"list"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.List) == 0 {
		return nil, invalidResponse()
	}
	return result.List, nil
}

type ReimbursementRequest struct {
	Allocations map[string]any
	AssetID     string
	Time        int64
	Remark      string
	TagIDs      []string
}

func (c *Client) Reimburse(ctx context.Context, a Session, input ReimbursementRequest) ([]json.RawMessage, error) {
	raw, err := json.Marshal(input.Allocations)
	if err != nil {
		return nil, err
	}
	body := url.Values{"v": {string(raw)}, "bxtime": {strconv.FormatInt(input.Time, 10)}}
	if input.AssetID != "" && input.AssetID != "-1" {
		body.Set("did", input.AssetID)
	}
	if input.Remark != "" {
		body.Set("remark", input.Remark)
	}
	if len(input.TagIDs) > 0 {
		tags, _ := json.Marshal(input.TagIDs)
		body.Set("tags", string(tags))
	}
	data, err := c.post(ctx, "/baoxiao/baoxiao", a, body)
	if err != nil {
		return nil, err
	}
	var result struct {
		Bills []json.RawMessage `json:"bills"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Bills) == 0 {
		return nil, invalidResponse()
	}
	return result.Bills, nil
}
func (c *Client) CancelReimbursement(ctx context.Context, a Session, ids []string) error {
	numbers := make([]json.Number, len(ids))
	for i, id := range ids {
		numbers[i] = json.Number(id)
	}
	raw, _ := json.Marshal(numbers)
	_, err := c.post(ctx, "/baoxiao/cancelbaoxiao", a, url.Values{"v": {string(raw)}})
	return err
}
func (c *Client) UpgradeReimbursement(ctx context.Context, a Session) error {
	raw, err := c.post(ctx, "/baoxiao/upgradev2", a, url.Values{})
	if err != nil {
		return err
	}
	// The app's integer parser unwraps a sole "v" key; the upstream success value is {"v":1}.
	var wrapped map[string]json.RawMessage
	if json.Unmarshal(raw, &wrapped) == nil && len(wrapped) == 1 && wrapped["v"] != nil {
		raw = wrapped["v"]
	}
	if string(raw) == "1" {
		return nil
	}
	return invalidResponse()
}
