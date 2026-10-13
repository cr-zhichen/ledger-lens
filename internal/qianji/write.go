package qianji

import (
	"context"
	"encoding/json"
	"net/url"
)

// SyncBills follows qianji-mcp's /bill/syncall contract. Business IDs must be
// JSON numbers on the wire; callers use json.Number to retain all 64 bits.
type BillChanges struct {
	Changes []map[string]any `json:"changelist,omitempty"`
	Deletes []json.Number    `json:"dellist,omitempty"`
}

type WriteReceipt struct {
	Created   []string
	Updated   []string
	Deleted   []string
	Conflicts []string
	Failed    bool
}

func (c *Client) SyncBills(ctx context.Context, session Session, changes BillChanges) (WriteReceipt, error) {
	payload, err := json.Marshal(struct {
		Bills BillChanges `json:"bills"`
	}{changes})
	if err != nil {
		return WriteReceipt{}, err
	}
	data, err := c.post(ctx, "/bill/syncall", session, url.Values{"v": {string(payload)}})
	if err != nil {
		return WriteReceipt{}, err
	}
	var result struct {
		Sync struct {
			Bill *struct {
				Created   []json.RawMessage `json:"new_ids"`
				Updated   []json.RawMessage `json:"update_ids"`
				Deleted   []json.RawMessage `json:"del_ids"`
				Conflicts []json.RawMessage `json:"conf_ids"`
				Failed    json.RawMessage   `json:"has_failed"`
			} `json:"bill"`
		} `json:"sync_result"`
	}
	if json.Unmarshal(data, &result) != nil || result.Sync.Bill == nil {
		return WriteReceipt{}, invalidResponse()
	}
	bill := result.Sync.Bill
	receipt := WriteReceipt{}
	for _, group := range []struct {
		raw []json.RawMessage
		ids *[]string
	}{
		{bill.Created, &receipt.Created}, {bill.Updated, &receipt.Updated},
		{bill.Deleted, &receipt.Deleted}, {bill.Conflicts, &receipt.Conflicts},
	} {
		for _, raw := range group.raw {
			id, err := ID(raw, false)
			if err != nil {
				return WriteReceipt{}, err
			}
			*group.ids = append(*group.ids, id)
		}
	}
	// The upstream flag is a boolean or the integer 0/1, as in its current client.
	switch string(bill.Failed) {
	case "", "false", "0":
	case "true", "1":
		receipt.Failed = true
	default:
		return WriteReceipt{}, invalidResponse()
	}
	return receipt, nil
}

func (c *Client) BaseCurrency(ctx context.Context, session Session) (string, error) {
	data, err := c.Initialize(ctx, session)
	if err != nil {
		return "", err
	}
	var result struct {
		Config struct {
			Base string `json:"basecur"`
		} `json:"userconfigs"`
	}
	if json.Unmarshal(data, &result) != nil {
		return "", invalidResponse()
	}
	// An unset basecur means CNY in the current app protocol.
	if result.Config.Base == "" {
		return "CNY", nil
	}
	return result.Config.Base, nil
}

func (c *Client) Initialize(ctx context.Context, session Session) (json.RawMessage, error) {
	return c.post(ctx, "/client/init", session, url.Values{"v": {"0"}, "vvmark": {"0"}, "newinstall": {"0"}, "upgradealert": {"0"}})
}
