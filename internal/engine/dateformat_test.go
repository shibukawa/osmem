package engine

import (
	"strings"
	"testing"
	"time"
)

// Expectations below were recorded from OpenSearch 3.8.0.

func TestDefaultDateFormatParsing(t *testing.T) {
	df := ParseDateFormat(DefaultDateFormat)
	ok := map[string]string{
		"2024-01-15":                      "2024-01-15T00:00:00.000Z",
		"2024-01-15T10:00:00.123456789Z":  "2024-01-15T10:00:00.123Z",
		"-1":                              "1969-12-31T23:59:59.999Z",
		"00":                              "1970-01-01T00:00:00.000Z",
		"0000":                            "0000-01-01T00:00:00.000Z",
		"99999":                           "1970-01-01T00:01:39.999Z",
		"-99999":                          "-99999-01-01T00:00:00.000Z",
		"2024-01-15T10:00:00UTC":          "2024-01-15T10:00:00.000Z",
		"+12345-01-01":                    "+12345-01-01T00:00:00.000Z",
		"2024-01-15T10:00:00Europe/Paris": "2024-01-15T09:00:00.000Z",
		"1970-01-01T00:00:00.5+09:00:30":  "1969-12-31T14:59:30.500Z",
		"2024-01-15T10Z":                  "2024-01-15T10:00:00.000Z",
		"2024-01-15T10:00:00GMT+01:00":    "2024-01-15T09:00:00.000Z",
		"1705312800.5":                    "1970-01-20T17:41:52.800Z",
		"1705312800000.123456":            "2024-01-15T10:00:00.000Z",
		"2024-01-15T10:00:00.123+0900":    "2024-01-15T01:00:00.123Z",
		"2024-01-15T10:00:00,123Z":        "2024-01-15T10:00:00.123Z",
		"2024":                            "2024-01-01T00:00:00.000Z",
		"1970-01-01T00:00:00+14:00":       "1969-12-31T10:00:00.000Z",
		"2024-01-15T10:00:00-00:00":       "2024-01-15T10:00:00.000Z",
		"2024-01-15T":                     "2024-01-15T00:00:00.000Z",
		"-2024-01-15":                     "-2024-01-15T00:00:00.000Z",
		"2024-01-15T10:00:00+09":          "2024-01-15T01:00:00.000Z",
		"2024-01":                         "2024-01-01T00:00:00.000Z",
	}
	for in, want := range ok {
		got, err := df.ParseString(in)
		if err != nil {
			t.Errorf("%q: unexpected error %v", in, err)
			continue
		}
		if s := df.Format(got.UTC()); s != want {
			t.Errorf("%q: got %s want %s", in, s, want)
		}
	}
	for _, in := range []string{"2024/01/15", "2024-01-15TZ", "2024-01-15Z", "2024-01-15T10:00:00.Z", "2024-01-15T10:00:00+9",
		"2024-13-01", "2024-00-10", "12345-01-01", "1e3", "2024-02-30", "20240115T100000Z", "2024-1", "2024-01-1",
		"2024-01-15T10:00:00+24:00", "2024-01-15 10:00:00", " 2024-01-15", "10:00:00", "1e12", "2024-01-15T10:00:00.1234567891Z",
		"2024-01-15T10:00:60Z", "2024-01-15T1", "2024-01-15T10:0", "2024-01-15t10:00", "2024-01-15T10:00:00z", "+2024-01-15"} {
		_, err := df.ParseString(in)
		de, isDateErr := err.(*dateError)
		if !isDateErr || de.kind != dateErrParse || de.reason != "Failed to parse with all enclosed parsers" {
			t.Errorf("%q: want enclosed parsers failure, got %v", in, err)
		}
	}
	_, err := df.ParseString("-1705312800")
	if de, isDateErr := err.(*dateError); !isDateErr || de.kind != dateErrDateTime || de.reason != "Invalid value for Year (valid values -999999999 - 999999999): -1705312800" {
		t.Errorf("year range: %v", err)
	}
	_, err = df.ParseString("")
	if de, isDateErr := err.(*dateError); !isDateErr || de.kind != dateErrEmpty {
		t.Errorf("empty: %v", err)
	}
}

func TestCustomDateFormats(t *testing.T) {
	cases := []struct {
		format, in string
		millis     int64
		printed    string
	}{
		{"d/M/yyyy", "5/1/2024", 1704412800000, "5/1/2024"},
		{"yyyyMMdd", "20240115", 1705276800000, "20240115"},
		{"yy-MM-dd", "24-01-15", 1705276800000, "24-01-15"},
		{"MMM dd, yyyy", "Jan 15, 2024", 1705276800000, "Jan 15, 2024"},
		{"EEE, dd MMM yyyy HH:mm:ss Z", "Mon, 15 Jan 2024 10:00:00 +0900", 1705280400000, "Mon, 15 Jan 2024 01:00:00 +0000"},
		{"yyyy-MM-dd'T'HH:mm:ss.SSSZ", "2024-01-15T10:00:00.123+0900", 1705280400123, "2024-01-15T01:00:00.123+0000"},
		{"yyyy-MM-dd'T'HH:mm:ss.SSSXXX", "2024-01-15T10:00:00.123Z", 1705312800123, "2024-01-15T10:00:00.123Z"},
		{"yyyy-MM-dd HH:mm:ss.SSSSSS", "2024-01-15 10:00:00.123456", 1705312800123, "2024-01-15 10:00:00.123000"},
		{"hh:mm a yyyy-MM-dd", "10:00 PM 2024-01-15", 1705356000000, "10:00 PM 2024-01-15"},
		{"yyyy-MM-dd[ HH:mm]", "2024-01-15 10:30", 1705314600000, "2024-01-15 10:30"},
		{"yyyy-MM-dd[ HH:mm]", "2024-01-15", 1705276800000, "2024-01-15 00:00"},
		{"yyyy-DDD", "2024-015", 1705276800000, "2024-015"},
		{"epoch_second", "1705312800.123", 1705312800123, "1705312800.123"},
		{"date_optional_time", "2024-1-5", 1704412800000, "2024-01-05T00:00:00.000Z"},
		{"basic_date", "20240115", 1705276800000, "20240115"},
		{"date", "2024-1-15", 1705276800000, "2024-01-15"},
		{"year_month_day", "2024-1-15", 1705276800000, "2024-01-15"},
		{"date_hour_minute_second", "2024-01-15T10:00:00", 1705312800000, "2024-01-15T10:00:00"},
		{"yyyy-MM-dd||strict_date_optional_time", "2024-01-15T10:00:00Z", 1705312800000, "2024-01-15"},
		{"uuuu-MM-dd", "2024-01-15", 1705276800000, "2024-01-15"},
		{"YYYY-ww", "2024-03", 1705276800000, "2024-03"},
		{"rfc3339_lenient", "2024-01-15T10:00:00Z", 1705312800000, "2024-01-15T10:00:00.000Z"},
		{"epoch_millis", "-1.5", -2, "-2"},
	}
	for _, tc := range cases {
		df := ParseDateFormat(tc.format)
		got, err := df.ParseString(tc.in)
		if err != nil {
			t.Errorf("%s %q: %v", tc.format, tc.in, err)
			continue
		}
		if ms := epochMillis(got); ms != tc.millis {
			t.Errorf("%s %q: millis %d want %d", tc.format, tc.in, ms, tc.millis)
		}
		if s := df.Format(time.UnixMilli(epochMillis(got)).UTC()); s != tc.printed {
			t.Errorf("%s %q: printed %q want %q", tc.format, tc.in, s, tc.printed)
		}
	}
	rejected := []struct{ format, in, reason string }{
		{"yyyy-MM-dd", "2024-1-15", "Text '2024-1-15' could not be parsed at index 5"},
		{"yyyy-MM-dd'T'HH:mm:ss.SSS", "2024-01-15T10:00:00.12", "Text '2024-01-15T10:00:00.12' could not be parsed at index 20"},
		{"yyyy-MM-dd", "2024-01-15T", "Text '2024-01-15T' could not be parsed, unparsed text found at index 10"},
		{"yyyy-MM-dd", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz", "Text 'abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijkl...' could not be parsed at index 0"},
		{"dd/MM/yyyy HH:mm", "05/01/2024 24:00", ""},
		{"yyyyMMdd", "2024011", ""},
		{"yy-MM-dd", "1924-01-15", ""},
		{"MMM dd, yyyy", "jan 15, 2024", ""},
		{"yyyy", "2024x", ""},
		{"date_optional_time", "2024-01-15 10:00", ""},
		{"strict_date", "2024-1-15", ""},
		{"xxxx-'W'ww-e", "2024-W03-1", ""},
		{"yyyy-MM-dd", "-2024-01-15", ""},
		{"yyyy-MM-dd", "12024-01-15", ""},
		{"dd/MM/yyyy HH:mm", "15/01/2024 9:00", ""},
	}
	for _, tc := range rejected {
		_, err := ParseDateFormat(tc.format).ParseString(tc.in)
		de, isDateErr := err.(*dateError)
		if !isDateErr || de.kind != dateErrParse {
			t.Errorf("%s %q: want parse failure, got %v", tc.format, tc.in, err)
			continue
		}
		if tc.reason != "" && de.reason != tc.reason {
			t.Errorf("%s %q: reason %q want %q", tc.format, tc.in, de.reason, tc.reason)
		}
	}
	dateTimeErrors := []struct{ format, in, reason string }{
		{"MM/dd/yyyy||yyyy", "13/01/2024", "Invalid value for MonthOfYear (valid values 1 - 12): 13"},
		{"yyyy-MM-dd", "2024-02-30", "Invalid date 'FEBRUARY 30'"},
		{"yyyy-MM-dd", "2024-01-32", "Invalid value for DayOfMonth (valid values 1 - 28/31): 32"},
	}
	for _, tc := range dateTimeErrors {
		_, err := ParseDateFormat(tc.format).ParseString(tc.in)
		de, isDateErr := err.(*dateError)
		if !isDateErr || de.kind != dateErrDateTime || de.reason != tc.reason {
			t.Errorf("%s %q: want date_time_exception %q, got %v", tc.format, tc.in, tc.reason, err)
		}
	}
}

func TestDateFormatPatternErrors(t *testing.T) {
	cases := map[string]string{
		"no_such_named_format_xx": "Invalid format: [no_such_named_format_xx]: Unknown pattern letter: o",
		"bogus_name_q":            "Invalid format: [bogus_name_q]: Unknown pattern letter: b",
		"yyyy-MM-dd||":            "Cannot have empty element in multi date format pattern: yyyy-MM-dd||",
	}
	for in, want := range cases {
		_, err := parseDateFormatChecked(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v want %s", in, err, want)
		}
	}
	if _, err := parseDateFormatChecked(""); err == nil || !strings.Contains(err.Error(), "No date pattern provided") {
		t.Errorf("empty format: %v", err)
	}
}

func TestDateRoundUpParsing(t *testing.T) {
	df := ParseDateFormat(DefaultDateFormat)
	cases := map[string]time.Time{
		"2015-01":       time.Date(2015, 1, 1, 23, 59, 59, 999999999, time.UTC),
		"2015":          time.Date(2015, 1, 1, 23, 59, 59, 999999999, time.UTC),
		"2015-01-01T10": time.Date(2015, 1, 1, 10, 59, 59, 999999999, time.UTC),
		"1000":          time.Date(1000, 1, 1, 23, 59, 59, 999999999, time.UTC),
		"10000":         time.Unix(0, 10000*int64(time.Millisecond)+999999).UTC(),
	}
	for in, want := range cases {
		got, err := ParseDateMath(in, df, time.Now(), time.UTC, true)
		if err != nil || !got.Equal(want) {
			t.Errorf("%s: got %v %v want %v", in, got, err, want)
		}
	}
	got, err := ParseDateMath("2015", ParseDateFormat("yyyy"), time.Now(), time.UTC, true)
	if err != nil || !got.Equal(time.Date(2015, 1, 1, 23, 59, 59, 999999999, time.UTC)) {
		t.Errorf("yyyy round up: %v %v", got, err)
	}
	if _, err := ParseDateMath("now/x", df, time.Now(), time.UTC, false); err == nil || err.Error() != "unit [x] not supported for date math [/x]" {
		t.Errorf("date math unit: %v", err)
	}
	if got := plusMonths(time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC), 1); !got.Equal(time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("plusMonths: %v", got)
	}
}
