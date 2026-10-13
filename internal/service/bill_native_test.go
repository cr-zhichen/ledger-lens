package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ledger-lens/internal/fault"
)

func TestBillExclusionSwitchesCreateAndPatch(t *testing.T) {
	cases := []struct{ input, want string }{
		{`"exclude_from_income_expense":true`, "1"},
		{`"exclude_from_budget":true`, "2"},
		{`"exclude_from_income_expense":true,"exclude_from_budget":true`, "3"},
		{`"exclude_from_income_expense":false,"exclude_from_budget":false`, "0"},
	}
	inputs := make([]string, len(cases))
	for i, tc := range cases {
		inputs[i] = `{"type":0,"money":10,"category_id":"2",` + tc.input + `}`
	}
	f, svc, _ := newWriteFixture(t)
	created, err := svc.CreateBills(context.Background(), fixtureCreate(t, `[`+strings.Join(inputs, ",")+`]`))
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range cases {
		bill, err := decodeObject(created.Items[i].Bill)
		if err != nil {
			t.Fatal(err)
		}
		extra, _ := billExtra(bill)
		if !created.Items[i].Confirmed || textValue(extra["flag"]) != tc.want {
			t.Fatalf("wrong exclusion flags: %s", created.Items[i].Bill)
		}
	}
	id := created.Items[0].ID
	// Preserve an unrelated bit and an unrelated extra field while toggling
	// each supported switch independently, including explicit false.
	f.bills[id]["extra"] = map[string]any{"flag": number(7), "server_field": "preserve"}
	for _, patch := range []struct{ input, want string }{
		{`{"exclude_from_income_expense":false}`, "6"},
		{`{"exclude_from_budget":false}`, "4"},
		{`{"exclude_from_income_expense":true}`, "5"},
		{`{"remark":"unchanged switches"}`, "5"},
		{`{"exclude_from_budget":true}`, "7"},
	} {
		updated, err := svc.UpdateBills(context.Background(), []UpdateBill{{ID: id, Patch: fixturePatch(t, patch.input)}})
		if err != nil || !updated.Items[0].Confirmed {
			t.Fatalf("patch %s: %+v %v", patch.input, updated, err)
		}
		bill, _ := decodeObject(updated.Items[0].Bill)
		extra, _ := billExtra(bill)
		if textValue(extra["flag"]) != patch.want || extra["server_field"] != "preserve" || textValue(bill["money"]) != "10.00" {
			t.Fatalf("patch changed unrelated data: %s", updated.Items[0].Bill)
		}
	}
}

func TestNativeCurrencyCreateAndUpdateWithoutRates(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	ctx := context.Background()
	result, err := svc.CreateBills(ctx, fixtureCreate(t, `[{"type":0,"money":10.25,"discount":0.20,"asset_id":"9","category_id":"2","exclude_from_budget":true},{"type":1,"money":25.01,"asset_id":"10","category_id":"3","exclude_from_income_expense":true}]`))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct{ amount, gross, flag string }{{"10.05", "10.25", "2"}, {"25.01", "25.01", "1"}} {
		bill, _ := decodeObject(result.Items[i].Bill)
		extra, _ := billExtra(bill)
		curr := extra["curr"].(map[string]any)
		if !result.Items[i].Confirmed || textValue(bill["money"]) != want.amount || textValue(extra["flag"]) != want.flag ||
			curr["ss"] != "USD" || curr["ts"] != "USD" || textValue(curr["sv"]) != want.gross || textValue(curr["tv"]) != want.amount || len(curr) != 4 {
			t.Fatalf("wrong native amounts: %s", result.Items[i].Bill)
		}
	}
	id := result.Items[0].ID
	// Represent an app-created historical snapshot. A metadata-only update must
	// keep it; an amount update must not leave that stale conversion attached.
	curr := f.bills[id]["extra"].(map[string]any)["curr"].(map[string]any)
	curr["bs"], curr["bv"] = "CNY", number(70)
	result, err = svc.UpdateBills(ctx, []UpdateBill{{ID: id, Patch: fixturePatch(t, `{"exclude_from_income_expense":true,"remark":"keep conversion"}`)}})
	if err != nil || !strings.Contains(string(result.Items[0].Bill), `"bv":70`) {
		t.Fatalf("metadata update lost conversion: %+v %v", result, err)
	}
	result, err = svc.UpdateBills(ctx, []UpdateBill{{ID: id, Patch: fixturePatch(t, `{"money":10.25,"discount":0.20}`)}})
	if err != nil || !strings.Contains(string(result.Items[0].Bill), `"bv":70`) {
		t.Fatalf("unchanged amount lost historical conversion: %+v %v", result, err)
	}
	result, err = svc.UpdateBills(ctx, []UpdateBill{{ID: id, Patch: fixturePatch(t, `{"money":20.01}`)}})
	if err != nil || !result.Items[0].Confirmed {
		t.Fatalf("native update: %+v %v", result, err)
	}
	bill, _ := decodeObject(result.Items[0].Bill)
	extra, _ := billExtra(bill)
	curr = extra["curr"].(map[string]any)
	if textValue(bill["money"]) != "19.81" || textValue(curr["sv"]) != "20.01" || textValue(curr["tv"]) != "19.81" || len(curr) != 4 || textValue(extra["flag"]) != "3" {
		t.Fatalf("amount update used conversion or lost flags: %s", result.Items[0].Bill)
	}
	result, err = svc.UpdateBills(ctx, []UpdateBill{{ID: id, Patch: fixturePatch(t, `{"asset_id":"89","discount":0}`)}})
	if err != nil || !result.Items[0].Confirmed || !strings.Contains(string(result.Items[0].Bill), `"money":20.01`) {
		t.Fatalf("same-currency hidden asset/discount update: %+v %v", result, err)
	}
	if f.rateReads != 0 {
		t.Fatal("native writes looked up exchange rates")
	}
}

func TestOptionalBaseAmountCreateAndUpdate(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	f.baseCurrency = "EUR"
	ctx := context.Background()
	created, err := svc.CreateBills(ctx, fixtureCreate(t, `[{"type":0,"money":10.25,"discount":0.20,"base_amount":9.13,"asset_id":"9","category_id":"2","exclude_from_budget":true}]`))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Items[0].ID
	check := func(result WriteResult, wantNet, wantGross, wantBase string) {
		t.Helper()
		bill, _ := decodeObject(result.Items[0].Bill)
		extra, _ := billExtra(bill)
		curr := extra["curr"].(map[string]any)
		if !result.Items[0].Confirmed || textValue(bill["money"]) != wantNet || textValue(curr["sv"]) != wantGross || textValue(curr["tv"]) != wantNet ||
			curr["ss"] != "USD" || curr["ts"] != "USD" || textValue(extra["flag"]) != "2" {
			t.Fatalf("native amount or metadata changed: %s", result.Items[0].Bill)
		}
		if wantBase == "" {
			if _, exists := curr["bs"]; exists {
				t.Fatalf("base currency remains: %s", result.Items[0].Bill)
			}
			if _, exists := curr["bv"]; exists {
				t.Fatalf("base amount remains: %s", result.Items[0].Bill)
			}
		} else if curr["bs"] != "EUR" || textValue(curr["bv"]) != wantBase {
			t.Fatalf("manual conversion missing or recomputed: %s", result.Items[0].Bill)
		}
	}
	check(created, "10.05", "10.25", "9.13")
	for _, tc := range []struct{ patch, net, gross, base string }{
		{`{"base_amount":9.12}`, "10.05", "10.25", "9.12"},
		{`{"money":20.01,"base_amount":18.07}`, "19.81", "20.01", "18.07"},
		{`{"remark":"preserve manual conversion"}`, "19.81", "20.01", "18.07"},
		{`{"discount":0}`, "20.01", "20.01", ""},
		{`{"base_amount":9999999999.99}`, "20.01", "20.01", "9999999999.99"},
		{`{"discount":20.01,"base_amount":0}`, "0.00", "20.01", "0.00"},
	} {
		result, err := svc.UpdateBills(ctx, []UpdateBill{{ID: id, Patch: fixturePatch(t, tc.patch)}})
		if err != nil {
			t.Fatalf("patch %s: %v", tc.patch, err)
		}
		check(result, tc.net, tc.gross, tc.base)
	}
}

func TestBaseAmountAddsCurrencyToExistingNativeBill(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	f.bills["100"] = fixtureBill(t, "100")
	f.bills["100"]["assetid"] = number(9)
	extra := f.bills["100"]["extra"].(map[string]any)
	extra["transfee"] = json.Number("-0.25")
	result, err := svc.UpdateBills(context.Background(), []UpdateBill{{ID: "100", Patch: fixturePatch(t, `{"base_amount":71.72}`)}})
	if err != nil {
		t.Fatal(err)
	}
	bill, _ := decodeObject(result.Items[0].Bill)
	extra, _ = billExtra(bill)
	curr := extra["curr"].(map[string]any)
	if !result.Items[0].Confirmed || textValue(bill["money"]) != "10.25" || textValue(curr["sv"]) != "10.50" || curr["ss"] != "USD" || curr["ts"] != "USD" || textValue(curr["tv"]) != "10.25" || curr["bs"] != "CNY" || textValue(curr["bv"]) != "71.72" {
		t.Fatalf("wrong conversion for existing native bill: %s", result.Items[0].Bill)
	}
}

func TestBaseAmountRejectsBaseCurrencyAndRelatedBills(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	for _, asset := range []string{"-1", "8", "12"} {
		_, err := svc.CreateBills(context.Background(), fixtureCreate(t, `[{"type":0,"money":10,"base_amount":10,"asset_id":"`+asset+`","category_id":"2"}]`))
		if !fault.Is(err, "INVALID_ARGUMENT") || f.writes != 0 {
			t.Fatalf("base-currency conversion accepted: %v", err)
		}
	}
	f.bills["100"] = fixtureBill(t, "100")
	f.bills["100"]["assetid"] = number(9)
	f.bills["100"]["extra"] = map[string]any{"rfds": map[string]any{"101": number(1)}}
	_, err := svc.UpdateBills(context.Background(), []UpdateBill{{ID: "100", Patch: fixturePatch(t, `{"base_amount":72}`)}})
	if !fault.Is(err, "RELATED_BILLS_EXIST") || f.writes != 0 {
		t.Fatalf("related financial patch accepted: %v", err)
	}
}

func TestBaseCurrencyPairRequiredForInheritedSnapshots(t *testing.T) {
	for _, fields := range []string{`"bs":"CNY"`, `"bv":70`, `"bs":"","bv":70`, `"bs":"CNY","bv":null`, `"bs":"CNY","bv":-1`} {
		t.Run(fields, func(t *testing.T) {
			f, svc, _ := newWriteFixture(t)
			f.bills["100"] = fixtureBill(t, "100")
			f.bills["100"]["assetid"] = number(9)
			f.bills["100"]["extra"], _ = decodeObject([]byte(`{"curr":{"ss":"USD","sv":10.25,` + fields + `}}`))
			_, err := svc.UpdateBills(context.Background(), []UpdateBill{{ID: "100", Patch: fixturePatch(t, `{"remark":"keep amounts"}`)}})
			if !fault.Is(err, "RESPONSE_INVALID") || f.writes != 0 {
				t.Fatalf("invalid inherited conversion written: %v", err)
			}
		})
	}
}

func TestBaseAmountConfirmationRejectsMissingOrChangedConversion(t *testing.T) {
	for _, mode := range []string{"drop-base-symbol", "stale-base-currency"} {
		t.Run(mode, func(t *testing.T) {
			f, svc, _ := newWriteFixture(t)
			f.mode = mode
			result, err := svc.CreateBills(context.Background(), fixtureCreate(t, `[{"type":0,"money":10.25,"base_amount":71.72,"asset_id":"9","category_id":"2"}]`))
			if !fault.Is(err, "WRITE_CONFIRMATION_FAILED") || f.writes != 1 || result.Items[0].Confirmed {
				t.Fatalf("invalid server conversion confirmed: %+v %v", result, err)
			}
		})
	}
}

func TestCurrencyUpdateRequiresTargetAmount(t *testing.T) {
	for _, asset := range []string{"8", "11", "-1", "12"} {
		t.Run(asset, func(t *testing.T) {
			f, svc, _ := newWriteFixture(t)
			f.bills["100"] = fixtureBill(t, "100")
			f.bills["100"]["assetid"] = number(9)
			_, err := svc.UpdateBills(context.Background(), []UpdateBill{{ID: "100", Patch: fixturePatch(t, `{"asset_id":"`+asset+`","money":20}`)}})
			if !fault.Is(err, "INVALID_ARGUMENT") || f.writes != 0 {
				t.Fatalf("currency was reinterpreted: %v", err)
			}
		})
	}
	f, svc, _ := newWriteFixture(t)
	f.bills["100"] = fixtureBill(t, "100")
	f.bills["100"]["assetid"] = number(8)
	f.bills["100"]["extra"] = map[string]any{"curr": map[string]any{"ss": "USD", "sv": number(10), "ts": "CNY", "tv": number(70)}}
	_, err := svc.UpdateBills(context.Background(), []UpdateBill{{ID: "100", Patch: fixturePatch(t, `{"money":20}`)}})
	if !fault.Is(err, "INVALID_ARGUMENT") || f.writes != 0 {
		t.Fatalf("cross-currency write accepted: %v", err)
	}
}

func TestDiscountUpdateClearsConversionWithoutTargetFields(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	f.bills["100"] = fixtureBill(t, "100")
	f.bills["100"]["assetid"] = number(9)
	f.bills["100"]["extra"] = map[string]any{"flag": number(2), "curr": map[string]any{"ss": "USD", "sv": f.bills["100"]["money"], "bs": "CNY", "bv": number(70)}}
	result, err := svc.UpdateBills(context.Background(), []UpdateBill{{ID: "100", Patch: fixturePatch(t, `{"discount":1.25}`)}})
	if err != nil || !result.Items[0].Confirmed {
		t.Fatalf("discount update: %+v %v", result, err)
	}
	bill, _ := decodeObject(result.Items[0].Bill)
	extra, _ := billExtra(bill)
	curr := extra["curr"].(map[string]any)
	if textValue(bill["money"]) != "9.00" || textValue(curr["sv"]) != "10.25" || len(curr) != 4 || textValue(extra["flag"]) != "2" {
		t.Fatalf("discount kept a stale conversion or changed original amount: %s", result.Items[0].Bill)
	}
}

func TestNativeCurrencyUsesAccountBaseAndEmptyAssetCurrency(t *testing.T) {
	f, svc, _ := newWriteFixture(t)
	f.baseCurrency = "USD"
	created, err := svc.CreateBills(context.Background(), fixtureCreate(t, `[{"type":0,"money":10,"asset_id":"12","category_id":"2"},{"type":0,"money":10,"asset_id":"8","category_id":"2"}]`))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := decodeObject(created.Items[0].Bill)
	extra, _ := billExtra(first)
	if extra["curr"] != nil {
		t.Fatal("empty asset currency did not use USD base")
	}
	second, _ := decodeObject(created.Items[1].Bill)
	extra, _ = billExtra(second)
	if extra["curr"].(map[string]any)["ss"] != "CNY" {
		t.Fatal("CNY asset was treated as USD")
	}
	updated, err := svc.UpdateBills(context.Background(), []UpdateBill{{ID: created.Items[0].ID, Patch: fixturePatch(t, `{"asset_id":"9","money":12.34}`)}})
	if err != nil || !updated.Items[0].Confirmed {
		t.Fatalf("same USD currency rejected: %+v %v", updated, err)
	}
}

func TestWriteConfirmationChecksFlagsAndNativeCurrency(t *testing.T) {
	for _, mode := range []string{"drop-flags", "wrong-currency", "stale-base-currency"} {
		t.Run(mode, func(t *testing.T) {
			f, svc, _ := newWriteFixture(t)
			f.mode = mode
			result, err := svc.CreateBills(context.Background(), fixtureCreate(t, `[{"type":0,"money":10,"asset_id":"9","category_id":"2","exclude_from_budget":true}]`))
			if !fault.Is(err, "WRITE_CONFIRMATION_FAILED") || len(result.Items) != 1 || result.Items[0].Confirmed || f.writes != 1 {
				t.Fatalf("incorrect remote result confirmed: %+v %v", result, err)
			}
		})
	}
	f, svc, _ := newWriteFixture(t)
	f.bills["100"] = fixtureBill(t, "100")
	f.mode = "omit-empty-extra"
	result, err := svc.UpdateBills(context.Background(), []UpdateBill{{ID: "100", Patch: fixturePatch(t, `{"exclude_from_budget":false}`)}})
	if err != nil || !result.Items[0].Confirmed {
		t.Fatalf("omitted false flag rejected: %+v %v", result, err)
	}
}
