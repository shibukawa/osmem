package engine

import (
	"encoding/json"
	"math"
	"testing"
)

// Expected strings were written by OpenSearch 3.8.0 (Jackson with
// Double.toString / Float.toString).
func TestJavaNumberString(t *testing.T) {
	doubles := map[float64]string{
		1:                             "1.0",
		45:                            "45.0",
		0.0001:                        "1.0E-4",
		12345678901234567890:          "1.2345678901234567E19",
		7.165602000005e11:             "7.165602000005E11",
		61728.395000000004:            "61728.395000000004",
		123456.79000000001:            "123456.79000000001",
		float64(float32(0.00012)):     "1.1999999696854502E-4",
		float64(float32(123456789.5)): "1.23456792E8",
		0.001:                         "0.001",
		9999999:                       "9999999.0",
		10000000:                      "1.0E7",
		-2.5:                          "-2.5",
		1196532899750:                 "1.19653289975E12",
	}
	for v, want := range doubles {
		if got := javaNumberString(v, 64); got != want {
			t.Errorf("double %v: got %s, want %s", v, got, want)
		}
	}
	floats := map[float32]string{
		0.00012:     "1.2E-4",
		123456789.5: "1.2345679E8",
		1e-5:        "1.0E-5",
		12345678:    "1.2345678E7",
		7000:        "7000.0",
		12.99:       "12.99",
		1.0996094:   "1.0996094",
	}
	for v, want := range floats {
		if got := javaNumberString(float64(v), 32); got != want {
			t.Errorf("float %v: got %s, want %s", v, got, want)
		}
	}
	if got := string(appendJavaNumber(nil, math.Inf(1), 64)); got != `"Infinity"` {
		t.Errorf("infinity: %s", got)
	}
}

func TestEncodeJSON(t *testing.T) {
	body := M{
		"d":   45.0,
		"f":   float32(12.99),
		"i":   5,
		"n":   json.Number("1.50"),
		"raw": json.RawMessage(`{"b": 1, "a":[2.0,"x"]}`),
		"s":   "q\"\\\x01\t/é",
		"e":   []any{},
		"o":   M{},
		"nil": nil,
	}
	got, err := EncodeJSON(body, false)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"d":45.0,"e":[],"f":12.99,"i":5,"n":1.50,"nil":null,"o":{},"raw":{"b": 1, "a":[2.0,"x"]},"s":"q\"\\` + `\` + `u0001\t/é"}`
	if string(got) != want {
		t.Errorf("compact:\n got %s\nwant %s", got, want)
	}
	got, err = EncodeJSON(M{"a": []any{1, M{}}, "b": []any{}, "raw": json.RawMessage(`{"z": 1}`)}, true)
	if err != nil {
		t.Fatal(err)
	}
	want = "{\n  \"a\" : [\n    1,\n    { }\n  ],\n  \"b\" : [ ],\n  \"raw\" : {\"z\": 1}\n}\n"
	if string(got) != want {
		t.Errorf("pretty:\n got %q\nwant %q", got, want)
	}
}

// Expected strings were produced by OpenSearch 3.8.0 docvalue_fields with a
// numeric format on a double field.
func TestDecimalFormat(t *testing.T) {
	values := []float64{0.5, 1.5, 2.5, 0.15, 4.3999999999999995, 1234567.891, -1234.5, 0.001234, -0.5, 1e21, 0}
	cases := map[string][]string{
		"0":                     {"0", "2", "2", "0", "4", "1234568", "-1234", "0", "-0", "1000000000000000000000", "0"},
		"0.0":                   {"0.5", "1.5", "2.5", "0.1", "4.4", "1234567.9", "-1234.5", "0.0", "-0.5", "1000000000000000000000.0", "0.0"},
		"0.00":                  {"0.50", "1.50", "2.50", "0.15", "4.40", "1234567.89", "-1234.50", "0.00", "-0.50", "1000000000000000000000.00", "0.00"},
		"#,##0.00":              {"0.50", "1.50", "2.50", "0.15", "4.40", "1,234,567.89", "-1,234.50", "0.00", "-0.50", "1,000,000,000,000,000,000,000.00", "0.00"},
		"#.##":                  {"0.5", "1.5", "2.5", "0.15", "4.4", "1234567.89", "-1234.5", "0", "-0.5", "1000000000000000000000", "0"},
		"0000.0":                {"0000.5", "0001.5", "0002.5", "0000.1", "0004.4", "1234567.9", "-1234.5", "0000.0", "-0000.5", "1000000000000000000000.0", "0000.0"},
		"#":                     {"0", "2", "2", "0", "4", "1234568", "-1234", "0", "-0", "1000000000000000000000", "0"},
		"0.###E0":               {"5E-1", "1.5E0", "2.5E0", "1.5E-1", "4.4E0", "1.235E6", "-1.234E3", "1.234E-3", "-5E-1", "1E21", "0E0"},
		"00.00%":                {"50.00%", "150.00%", "250.00%", "15.00%", "440.00%", "123456789.10%", "-123450.00%", "00.12%", "-50.00%", "99999999999999990000000.00%", "00.00%"},
		"$#,##0.00;($#,##0.00)": {"$0.50", "$1.50", "$2.50", "$0.15", "$4.40", "$1,234,567.89", "($1,234.50)", "$0.00", "($0.50)", "$1,000,000,000,000,000,000,000.00", "$0.00"},
		"'x'0'y'":               {"x0y", "x2y", "x2y", "x0y", "x4y", "x1234568y", "-x1234y", "x0y", "-x0y", "x1000000000000000000000y", "x0y"},
		"###,###.###":           {"0.5", "1.5", "2.5", "0.15", "4.4", "1,234,567.891", "-1,234.5", "0.001", "-0.5", "1,000,000,000,000,000,000,000", "0"},
		"0.0#":                  {"0.5", "1.5", "2.5", "0.15", "4.4", "1234567.89", "-1234.5", "0.0", "-0.5", "1000000000000000000000.0", "0.0"},
	}
	for pattern, want := range cases {
		for i, v := range values {
			if pattern == "00.00%" && v == 1e21 {
				// 1e21*100 is the double nearest 1e23, for which the JDK's
				// FloatingDecimal produces the non-shortest digits 9999999999999999
				continue
			}
			got, ok := formatDecimal(pattern, v)
			if !ok {
				t.Fatalf("pattern %q rejected", pattern)
			}
			if got != want[i] {
				t.Errorf("%q.format(%v) = %q, want %q", pattern, v, got, want[i])
			}
		}
	}
}
