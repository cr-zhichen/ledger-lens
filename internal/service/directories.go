package service

import (
	"encoding/json"
	"strconv"
	"strings"

	"ledger-lens/internal/fault"
	"ledger-lens/internal/qianji"
)

const maxListPages = 50

// AllPages merges a paged directory into one {"list":[...]} object. The app
// does not document whether pages start at 0 or 1, so an empty page 0 is
// followed by page 1, and repeated items are merged by ID.
func AllPages(fetch func(page int) (qianji.ListPage, error)) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	items := []json.RawMessage{}
	seen := map[string]bool{}
	idle := 0
	for page := 0; page < maxListPages; page++ {
		result, err := fetch(page)
		if err != nil {
			return nil, err
		}
		if fields == nil {
			fields = result.Fields
		}
		added := 0
		for _, item := range result.Items {
			if key := itemKey(item); !seen[key] {
				seen[key] = true
				items = append(items, item)
				added++
			}
		}
		if added == 0 {
			idle++
		} else {
			idle = 0
		}
		if page == 0 && added == 0 {
			continue
		}
		// Two pages without new items mean the cursor is not advancing.
		if !result.HasMore || idle >= 2 {
			output := map[string]json.RawMessage{}
			for key, value := range fields {
				output[key] = value
			}
			delete(output, "hasmore")
			output["list"], _ = json.Marshal(items)
			output["pages"] = json.RawMessage(strconv.Itoa(page + 1))
			return json.Marshal(output)
		}
	}
	return nil, fault.New("LIST_LIMIT", "列表超过 50 页限制，请在钱迹中查看", 4)
}

func itemKey(item json.RawMessage) string {
	var object map[string]json.RawMessage
	if json.Unmarshal(item, &object) == nil {
		if id := strings.Trim(strings.TrimSpace(string(object["id"])), `"`); id != "" && id != "null" {
			return "id:" + id
		}
	}
	return "raw:" + string(item)
}
