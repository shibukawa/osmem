package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// settingDef describes a registered OpenSearch setting (settings_table.go).
type settingDef struct {
	// kind: b bool, i int, l long, d double, t time value, z byte size,
	// s string, L string list, p byte size or heap percentage, r ratio or
	// byte size, x validated by name (validateSpecialSetting).
	kind byte
	// flags: D dynamic, P private, I internal, F final.
	flags string
	def   any // string or []any
	min   string
	max   string
}

func (d settingDef) dynamic() bool  { return strings.Contains(d.flags, "D") }
func (d settingDef) private() bool  { return strings.Contains(d.flags, "P") }
func (d settingDef) internal() bool { return strings.Contains(d.flags, "I") }
func (d settingDef) final() bool    { return strings.Contains(d.flags, "F") }

var indexSettingDefs, clusterSettingDefs map[string]settingDef

func init() {
	indexSettingDefs = parseSettingTable(indexSettingTable)
	clusterSettingDefs = parseSettingTable(clusterSettingTable)
	// removed in OpenSearch 3, but osmem keeps it: while it is set GET /
	// mimics Elasticsearch 7.10.2 for older clients
	clusterSettingDefs["compatibility.override_main_response_version"] = settingDef{kind: 'b', flags: "D", def: "false"}
}

func parseSettingTable(table string) map[string]settingDef {
	out := map[string]settingDef{}
	for _, line := range strings.Split(table, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 6 {
			continue
		}
		d := settingDef{kind: f[1][0], flags: f[2], min: f[4], max: f[5]}
		var def any
		if err := json.Unmarshal([]byte(f[3]), &def); err == nil {
			d.def = def
		}
		out[f[0]] = d
	}
	return out
}

// lookupIndexSetting finds the definition of an index setting, including the
// group and affix settings that have no entry of their own.
func lookupIndexSetting(key string) (settingDef, bool) {
	if d, ok := indexSettingDefs[key]; ok {
		return d, true
	}
	switch {
	case strings.HasPrefix(key, "index.analysis."):
		return settingDef{kind: 's'}, true
	case strings.HasPrefix(key, "index.similarity."):
		return settingDef{kind: 'x'}, true
	case strings.HasPrefix(key, "index.routing.allocation.include."), strings.HasPrefix(key, "index.routing.allocation.exclude."),
		strings.HasPrefix(key, "index.routing.allocation.require."):
		return settingDef{kind: 's', flags: "D"}, true
	case strings.HasPrefix(key, "index.routing.allocation.initial_recovery."):
		return settingDef{kind: 's', flags: "P"}, true
	case strings.HasPrefix(key, "index.ingestion_source.param."):
		return settingDef{kind: 's'}, true
	}
	return settingDef{}, false
}

// lookupClusterSetting finds a dynamic cluster setting.
func lookupClusterSetting(key string) (settingDef, bool) {
	if d, ok := clusterSettingDefs[key]; ok {
		return d, true
	}
	switch {
	case strings.HasPrefix(key, "logger."):
		return settingDef{kind: 'x', flags: "D"}, true
	case strings.HasPrefix(key, "cluster.routing.allocation.include."), strings.HasPrefix(key, "cluster.routing.allocation.exclude."),
		strings.HasPrefix(key, "cluster.routing.allocation.require."), strings.HasPrefix(key, "cluster.routing.allocation.awareness.force."),
		strings.HasPrefix(key, "cluster.remote."), strings.HasPrefix(key, "search.remote."), strings.HasPrefix(key, "script.context."):
		return settingDef{kind: 's', flags: "D"}, true
	}
	return settingDef{}, false
}

// setting values ---------------------------------------------------------

// settingValue converts a JSON value to the form Settings keeps it in:
// scalars become strings (numbers keep their JSON text, decimals the Java
// double rendering), arrays become string lists and null stays nil.
func settingValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = settingString(e)
		}
		return out
	}
	return settingString(v)
}

func settingString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		s := t.String()
		if strings.ContainsAny(s, ".eE") {
			if f, err := t.Float64(); err == nil {
				return javaNumberString(f, 64)
			}
		}
		return s
	case float64:
		return javaNumberString(t, 64)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = settingString(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case M:
		b, _ := json.Marshal(t)
		return string(b)
	}
	return fmt.Sprint(v)
}

// scalarSetting returns the string a scalar setting parser sees (a list
// renders the way Java's List.toString does).
func scalarSetting(v any) string {
	if l, ok := v.([]any); ok {
		return settingString(l)
	}
	s, _ := v.(string)
	return s
}

// flattenSettingsBody flattens a settings object into dotted keys
// (Settings.Builder.loadFromMap).
func flattenSettingsBody(dst map[string]any, prefix string, m M) {
	for k, v := range m {
		if sub, ok := v.(M); ok {
			flattenSettingsBody(dst, prefix+k+".", sub)
			continue
		}
		dst[prefix+k] = settingValue(v)
	}
}

// normalizeIndexSettingKeys adds the index. prefix to keys without it.
func normalizeIndexSettingKeys(flat map[string]any) map[string]any {
	out := make(map[string]any, len(flat))
	for k, v := range flat {
		if !strings.HasPrefix(k, "index.") {
			k = "index." + k
		}
		out[k] = v
	}
	return out
}

// flatIndexSettings flattens stored (nested) index settings.
func flatIndexSettings(nested M) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, m M)
	walk = func(prefix string, m M) {
		for k, v := range m {
			if sub, ok := v.(M); ok {
				walk(prefix+k+".", sub)
				continue
			}
			out[prefix+k] = v
		}
	}
	walk("", nested)
	return out
}

// nestSettings builds the structured form of flat settings
// (Settings.getAsStructuredMap): a key that is both a value and a prefix of
// other keys keeps dotted notation for the longer keys.
func nestSettings(flat map[string]any) M {
	keys := make([]string, 0, len(flat))
	for k := range flat {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := M{}
	for _, k := range keys {
		putStructured(out, k, flat[k])
	}
	return out
}

func putStructured(m M, key string, v any) {
	dot := strings.IndexByte(key, '.')
	if dot < 0 {
		if inner, ok := m[key].(M); ok {
			for ik, iv := range inner {
				m[key+"."+ik] = iv
			}
		}
		m[key] = v
		return
	}
	head, rest := key[:dot], key[dot+1:]
	switch existing := m[head].(type) {
	case nil:
		inner := M{}
		putStructured(inner, rest, v)
		m[head] = inner
	case M:
		putStructured(existing, rest, v)
	default:
		m[key] = v
	}
}

// renderSettings renders flat settings nested or flat (flat_settings).
func renderSettings(flat map[string]any, flatOut bool) M {
	if flatOut {
		out := make(M, len(flat))
		for k, v := range flat {
			out[k] = v
		}
		return out
	}
	return nestSettings(flat)
}

// settingNamesMatch is Regex.simpleMatch over the requested setting names.
func settingNamesMatch(names []string, key string) bool {
	if len(names) == 0 {
		return true
	}
	for _, n := range names {
		if simpleMatch(n, key) {
			return true
		}
	}
	return false
}

// errors --------------------------------------------------------------------

func errSettings(reason string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "settings_exception", Reason: reason}
}

func settingNumberFormatErr(key, value string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "Failed to parse value [" + value + "] for setting [" + key + "]",
		Cause: &Error{Type: "number_format_exception", Reason: `For input string: "` + value + `"`}}
}

// settingParseErr is an OpenSearchParseException rethrown by Setting.get as
// an IllegalArgumentException with the same message.
func settingParseErr(reason string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: reason, Cause: &Error{Type: "parse_exception", Reason: reason}}
}

// errUnknownIndexSetting is the settings_exception of an unregistered index
// setting, with OpenSearch's "did you mean" suggestions.
func errUnknownIndexSetting(key string) *Error {
	msg := "unknown setting [" + key + "]"
	type scored struct {
		score float32
		key   string
	}
	var candidates []scored
	for k := range indexSettingDefs {
		if s := levenshteinSimilarity(key, k); s > 0.7 {
			candidates = append(candidates, scored{s, k})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].key < candidates[j].key
	})
	switch len(candidates) {
	case 0:
		msg += " please check that any required plugins are installed, or check the breaking changes documentation for removed settings"
	case 1:
		msg += " did you mean [" + candidates[0].key + "]?"
	default:
		keys := make([]string, len(candidates))
		for i, c := range candidates {
			keys[i] = c.key
		}
		msg += " did you mean any of [" + strings.Join(keys, ", ") + "]?"
	}
	return errSettings(msg)
}

// javaHashSetOrder returns the iteration order of a java.util.HashSet the
// strings were added to in order (bucket order of the final table, insertion
// order within a bucket).
func javaHashSetOrder(items []string) []string {
	var uniq []string
	seen := map[string]bool{}
	for _, s := range items {
		if !seen[s] {
			seen[s] = true
			uniq = append(uniq, s)
		}
	}
	n := 16
	for len(uniq) > n*3/4 {
		n *= 2
	}
	bucket := func(s string) int {
		var h int32
		for _, u := range utf16.Encode([]rune(s)) {
			h = 31*h + int32(u)
		}
		h ^= int32(uint32(h) >> 16)
		return int(uint32(h) & uint32(n-1))
	}
	out := append([]string(nil), uniq...)
	sort.SliceStable(out, func(i, j int) bool { return bucket(out[i]) < bucket(out[j]) })
	return out
}

// value parsers ---------------------------------------------------------------

var javaDoubleRe = regexp.MustCompile(`^[+-]?(NaN|Infinity|((\d+\.?\d*|\.\d+)([eE][+-]?\d+)?)[fFdD]?)$`)

// parseSettingDouble is Double.parseDouble for decimal input.
func parseSettingDouble(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	if !javaDoubleRe.MatchString(t) {
		return 0, false
	}
	t = strings.TrimRight(t, "fFdD")
	switch strings.TrimLeft(t, "+-") {
	case "NaN":
		return math.NaN(), true
	case "Infinity":
		if strings.HasPrefix(t, "-") {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f, true
		}
		return 0, false
	}
	return f, true
}

var timeUnits = []struct {
	suffix string
	nanos  int64
}{{"nanos", 1}, {"micros", 1e3}, {"ms", 1e6}, {"s", 1e9}, {"m", 60e9}, {"h", 3600e9}, {"d", 86400e9}}

// parseTimeSetting is TimeValue.parseTimeValue; it returns the value in
// milliseconds (TimeValue.millis()).
func parseTimeSetting(key, value string) (int64, *Error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	for _, u := range timeUnits {
		matches := strings.HasSuffix(normalized, u.suffix)
		if u.suffix == "m" {
			// minutes are case sensitive ("M" would be months)
			matches = strings.HasSuffix(value, "m")
		}
		if !matches {
			continue
		}
		num := strings.TrimSpace(normalized[:len(normalized)-len(u.suffix)])
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			cause := &Error{Type: "number_format_exception", Reason: `For input string: "` + num + `"`}
			if _, isDouble := parseSettingDouble(num); isDouble {
				return 0, &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "failed to parse [" + value + "], fractional time values are not supported", Cause: cause}
			}
			return 0, &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "failed to parse [" + value + "]", Cause: cause}
		}
		if n < -1 {
			return 0, javaIllegalArgument("failed to parse setting [" + key + "] with value [" + value + "] as a time value: negative durations are not supported")
		}
		nanos := n * u.nanos
		if u.nanos > 1 && n != 0 && nanos/u.nanos != n {
			nanos = math.MaxInt64
		}
		return nanos / 1e6, nil
	}
	if regexp.MustCompile(`^-0*1$`).MatchString(normalized) {
		return -1, nil
	}
	if regexp.MustCompile(`^0+$`).MatchString(normalized) {
		return 0, nil
	}
	return 0, javaIllegalArgument("failed to parse setting [" + key + "] with value [" + value + "] as a time value: unit is missing or unrecognized")
}

var byteUnits = []struct {
	suffix string
	bytes  int64
}{{"k", 1 << 10}, {"kb", 1 << 10}, {"m", 1 << 20}, {"mb", 1 << 20}, {"g", 1 << 30}, {"gb", 1 << 30}, {"t", 1 << 40}, {"tb", 1 << 40},
	{"p", 1 << 50}, {"pb", 1 << 50}, {"b", 1}}

// parseBytesSetting is ByteSizeValue.parseBytesSizeValue.
func parseBytesSetting(key, value string) (int64, *Error) {
	lower := strings.TrimSpace(strings.ToLower(value))
	for _, u := range byteUnits {
		if !strings.HasSuffix(lower, u.suffix) {
			continue
		}
		num := strings.TrimSpace(lower[:len(lower)-len(u.suffix)])
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			if _, isDouble := parseSettingDouble(num); isDouble {
				return 0, settingParseErr("Failed to parse bytes value [" + value + "]. Fractional bytes values have been deprecated since Legacy 6.2. Use non-fractional bytes values instead: found for setting [" + key + "]")
			}
			return 0, settingParseErr("failed to parse [" + value + "]")
		}
		if n < -1 {
			return 0, javaIllegalArgument("Values less than -1 bytes are not supported: " + value)
		}
		return n * u.bytes, nil
	}
	switch lower {
	case "-1":
		return -1, nil
	case "0":
		return 0, nil
	}
	return 0, settingParseErr("failed to parse setting [" + key + "] with value [" + value + "] as a size in bytes: unit is missing or unrecognized")
}

func validRatio(value string) bool {
	if strings.HasSuffix(value, "%") {
		p, ok := parseSettingDouble(value[:len(value)-1])
		return ok && p >= 0 && p <= 100
	}
	r, ok := parseSettingDouble(value)
	return ok && r >= 0 && r <= 1
}

// validateSetting validates the value of a registered setting.
func validateSetting(key string, d settingDef, v any) *Error {
	if v == nil {
		return nil
	}
	if d.kind == 'L' || d.kind == 's' {
		return nil
	}
	if d.kind == 'x' {
		return validateSpecialSetting(key, v)
	}
	s := scalarSetting(v)
	switch d.kind {
	case 'b':
		if s != "true" && s != "false" {
			return javaIllegalArgument("Failed to parse value [" + s + "] as only [true] or [false] are allowed.")
		}
	case 'i', 'l':
		bits := 64
		if d.kind == 'i' {
			bits = 32
		}
		n, err := strconv.ParseInt(s, 10, bits)
		if err != nil {
			return settingNumberFormatErr(key, s)
		}
		if d.min != "" {
			if m, err := strconv.ParseInt(d.min, 10, 64); err == nil && n < m {
				return javaIllegalArgument("Failed to parse value [" + s + "] for setting [" + key + "] must be >= " + d.min)
			}
		}
		if d.max != "" {
			if m, err := strconv.ParseInt(d.max, 10, 64); err == nil && n > m {
				return javaIllegalArgument("Failed to parse value [" + s + "] for setting [" + key + "] must be <= " + d.max)
			}
		}
	case 'd':
		f, ok := parseSettingDouble(s)
		if !ok {
			return settingNumberFormatErr(key, s)
		}
		if d.min != "" {
			if m, ok := parseSettingDouble(d.min); ok && f < m {
				return javaIllegalArgument("Failed to parse value [" + s + "] for setting [" + key + "] must be >= " + d.min)
			}
		}
		if d.max != "" {
			if m, ok := parseSettingDouble(d.max); ok && f > m {
				return javaIllegalArgument("Failed to parse value [" + s + "] for setting [" + key + "] must be <= " + d.max)
			}
		}
	case 't':
		ms, err := parseTimeSetting(key, s)
		if err != nil {
			return err
		}
		if d.min != "" {
			if m, err := parseTimeSetting(key, d.min); err == nil && ms < m {
				return javaIllegalArgument("failed to parse value [" + s + "] for setting [" + key + "], must be >= [" + d.min + "]")
			}
		}
		if d.max != "" {
			if m, err := parseTimeSetting(key, d.max); err == nil && ms > m {
				return javaIllegalArgument("failed to parse value [" + s + "] for setting [" + key + "], must be <= [" + d.max + "]")
			}
		}
	case 'z':
		if _, err := parseBytesSetting(key, s); err != nil {
			return err
		}
	case 'p':
		if strings.HasSuffix(s, "%") {
			num := s[:len(s)-1]
			p, ok := parseSettingDouble(num)
			if !ok {
				return settingParseErr("failed to parse [" + num + "] as a double")
			}
			if p < 0 || p > 100 {
				return settingParseErr("percentage should be in [0-100], got [" + num + "]")
			}
			return nil
		}
		if _, err := parseBytesSetting(key, s); err != nil {
			return err
		}
	case 'r':
		if !validRatio(s) {
			if _, err := parseBytesSetting(key, s); err != nil {
				return err
			}
		}
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	return false
}

func oneOfFold(value string, allowed ...string) bool {
	for _, a := range allowed {
		if strings.EqualFold(value, a) {
			return true
		}
	}
	return false
}

// validateSpecialSetting validates settings with dedicated parsers.
func validateSpecialSetting(key string, v any) *Error {
	var values []string
	if l, ok := v.([]any); ok {
		for _, e := range l {
			values = append(values, scalarSetting(e))
		}
	} else {
		s := scalarSetting(v)
		values = []string{s}
		if strings.HasPrefix(key, "index.sort.") {
			values = strings.Split(s, ",")
		}
	}
	s := scalarSetting(v)
	upper := strings.ToUpper(s)
	switch {
	case key == "index.codec":
		if !oneOf(s, "default", "lz4", "best_compression", "zlib") {
			return javaIllegalArgument("unknown value for [index.codec] must be one of [default, lz4, best_compression, zlib] but was: " + s)
		}
	case key == "index.codec.qatmode":
		if !oneOf(s, "auto", "hardware") {
			return javaIllegalArgument("Unknown value for [index.codec.qatmode] must be one of [auto, hardware] but was: " + s)
		}
	case strings.HasSuffix(key, "slowlog.level"):
		if !oneOf(upper, "TRACE", "DEBUG", "INFO", "WARN") {
			return javaIllegalArgument("No enum constant org.opensearch.common.logging.SlowLogLevel." + upper)
		}
	case key == "index.auto_expand_replicas", key == "index.auto_expand_search_replicas":
		if s == "false" {
			return nil
		}
		dash := strings.IndexByte(s, '-')
		bad := javaIllegalArgument(fmt.Sprintf("failed to parse [%s] from value: [%s] at index %d", key, s, dash))
		if dash < 0 {
			return bad
		}
		if _, err := strconv.ParseInt(s[:dash], 10, 32); err != nil {
			return bad
		}
		if rest := s[dash+1:]; rest != "all" {
			if _, err := strconv.ParseInt(rest, 10, 32); err != nil {
				return bad
			}
		}
	case key == "index.compound_format", key == "index.merge.log_byte_size_policy.no_cfs_ratio":
		if s == "true" || s == "false" {
			return nil
		}
		f, ok := parseSettingDouble(s)
		if !ok {
			return javaIllegalArgument("Expected a boolean or a value in the interval [0..1] but was: [" + s + "]")
		}
		if f < 0 || f > 1 {
			return javaIllegalArgument("NoCFSRatio must be in the interval [0..1] but was: [" + javaNumberString(f, 64) + "]")
		}
	case key == "index.fielddata.cache":
		if !oneOf(s, "node", "none") {
			return javaIllegalArgument("failed to parse [" + s + "] must be one of [node,none]")
		}
	case key == "index.merge.policy":
		if !oneOf(s, "tiered", "log_byte_size", "default") {
			return javaIllegalArgument("The setting has unsupported policy specified: " + s + ". Please use one of: tiered, log_byte_size, default")
		}
	case key == "index.merge_on_flush.policy":
		if !oneOf(s, "", "default", "merge-on-flush") {
			return javaIllegalArgument("The index.merge_on_flush.policy has unsupported policy specified: " + s + ". Please use one of: default, merge-on-flush")
		}
	case key == "index.replication.type":
		if !oneOf(s, "DOCUMENT", "SEGMENT") {
			return javaIllegalArgument("Could not parse ReplicationStrategy for [" + s + "]")
		}
	case key == "index.routing.allocation.enable", key == "cluster.routing.allocation.enable":
		if !oneOfFold(s, "all", "primaries", "new_primaries", "none") {
			return javaIllegalArgument("Illegal allocation.enable value [" + upper + "]")
		}
	case key == "index.routing.rebalance.enable", key == "cluster.routing.rebalance.enable":
		if !oneOfFold(s, "all", "primaries", "replicas", "none") {
			return javaIllegalArgument("Illegal rebalance.enable value [" + upper + "]")
		}
	case key == "cluster.routing.allocation.allow_rebalance":
		if !oneOfFold(s, "always", "indices_primaries_active", "indices_all_active") {
			return javaIllegalArgument("Illegal value for [" + key + "] [" + s + "]")
		}
	case key == "index.search.concurrent_segment_search.mode", key == "search.concurrent_segment_search.mode":
		if !oneOf(s, "all", "none", "auto") {
			return javaIllegalArgument("Setting value must be one of [all, none, auto]")
		}
	case key == "search.concurrent_segment_search.partition_strategy":
		if !oneOf(s, "segment", "balanced", "force") {
			return javaIllegalArgument("Setting value must be one of [segment, balanced, force]")
		}
	case key == "index.shard.check_on_startup":
		if !oneOf(s, "true", "false", "checksum") {
			return javaIllegalArgument("unknown value for [index.shard.check_on_startup] must be one of [true, false, checksum] but was: " + s)
		}
	case key == "index.store.data_locality":
		if !oneOf(upper, "FULL", "PARTIAL") {
			return javaIllegalArgument("Unknown locality type constant [" + s + "].")
		}
	case key == "index.store.fs.fs_lock":
		if !oneOf(s, "native", "simple") {
			return javaIllegalArgument(`unrecognized [index.store.fs.fs_lock] "` + s + `": must be native or simple`)
		}
	case key == "index.store.type":
		if !oneOf(s, "", "fs", "niofs", "mmapfs", "hybridfs", "simplefs", "remote_snapshot") {
			return javaIllegalArgument("Unknown store type [" + s + "]")
		}
	case key == "index.store.factory":
		if s != "" {
			return javaIllegalArgument("Unknown store factory [" + s + "]")
		}
	case key == "index.translog.durability":
		if !oneOf(upper, "REQUEST", "ASYNC") {
			return javaIllegalArgument("No enum constant org.opensearch.index.translog.Translog.Durability." + upper)
		}
	case key == "index.write.wait_for_active_shards":
		if s == "all" || s == "index-setting" {
			return nil
		}
		n, err := strconv.ParseInt(s, 10, 32)
		if err != nil {
			return &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "cannot parse ActiveShardCount[" + s + "]",
				Cause: &Error{Type: "number_format_exception", Reason: `For input string: "` + s + `"`}}
		}
		if n < 0 {
			return javaIllegalArgument("shard count cannot be a negative value")
		}
	case key == "index.composite_store.type":
		if !oneOf(s, "", "default") {
			return javaIllegalArgument("Unknown composite store type [" + s + "]")
		}
	case key == "index.recovery.type":
		if s != "" {
			return javaIllegalArgument("Unknown recovery type [" + s + "]")
		}
	case key == "index.data_path":
		if s != "" {
			return &Error{Status: http.StatusBadRequest, Type: "validation_exception", Reason: "Validation Failed: 1: path.shared_data must be set in order to use custom data paths;"}
		}
	case key == "index.indexing.slowlog.source":
		if s == "true" || s == "false" {
			return nil
		}
		if _, err := strconv.ParseInt(s, 10, 32); err != nil {
			return javaIllegalArgument("Failed to parse value [" + s + "] as only [true] or [false] are allowed.")
		}
	case key == "index.sort.order":
		for _, o := range values {
			if !oneOf(o, "asc", "desc") {
				return javaIllegalArgument("Illegal sort order:" + o)
			}
		}
	case key == "index.sort.mode":
		for _, o := range values {
			if !oneOf(o, "min", "max") {
				return javaIllegalArgument("Illegal sort mode: " + o)
			}
		}
	case key == "index.sort.missing":
		for _, o := range values {
			if !oneOf(o, "_last", "_first") {
				return javaIllegalArgument("Illegal missing value:[" + o + "], must be one of [_last, _first]")
			}
		}
	case key == "index.searchable_snapshot.shard_path_type":
		if !oneOf(s, "FIXED", "HASHED_PREFIX", "HASHED_INFIX") {
			return javaIllegalArgument("Could not parse PathType for [" + s + "]")
		}
	case key == "index.ingestion_source.error_strategy":
		if !oneOfFold(s, "DROP", "BLOCK") {
			return javaIllegalArgument("Invalid ingestion errorStrategy: " + s)
		}
	case key == "index.ingestion_source.mapper_type":
		if !oneOf(s, "default", "raw_payload", "field_mapping") {
			return javaIllegalArgument("Unknown ingestion mapper type: " + s + ". Valid values are: default, raw_payload, field_mapping")
		}
	case key == "index.mapping.depth.limit", key == "index.mapping.field_name_length.limit":
		limit, property := int64(1000), "opensearch.xcontent.depth.max"
		if key == "index.mapping.field_name_length.limit" {
			limit, property = 50000, "opensearch.xcontent.name.length.max"
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return settingNumberFormatErr(key, s)
		}
		if n < 1 {
			return javaIllegalArgument("Failed to parse value [" + s + "] for setting [" + key + "] must be >= 1")
		}
		if n > limit {
			return javaIllegalArgument(fmt.Sprintf("The provided value %d of the index setting '%s' exceeds per-JVM configured limit of %d. Please change the setting value or increase per-JVM limit using '%s' system property.", n, key, limit, property))
		}
	case strings.HasPrefix(key, "index.similarity.") && strings.HasSuffix(key, ".type"):
		name := strings.TrimSuffix(strings.TrimPrefix(key, "index.similarity."), ".type")
		if !oneOf(s, "BM25", "boolean", "DFR", "DFI", "IB", "LMDirichlet", "LMJelinekMercer", "scripted", "classic") {
			return javaIllegalArgument("Unknown Similarity type [" + s + "] for [" + name + "]")
		}
	case strings.HasPrefix(key, "logger."):
		if !oneOf(upper, "OFF", "FATAL", "ERROR", "WARN", "INFO", "DEBUG", "TRACE", "ALL") {
			return javaIllegalArgument("Unknown level constant [" + upper + "].")
		}
	case key == "cluster.no_cluster_manager_block", key == "cluster.no_master_block":
		if !oneOf(s, "all", "write", "metadata_write") {
			return javaIllegalArgument("invalid no-cluster-manager block [" + s + "], must be one of [all, write, metadata_write]")
		}
	case key == "script.max_compilations_rate":
		if s == "use-context" || s == "unlimited" {
			return nil
		}
		slash := strings.IndexByte(s, '/')
		ok := slash > 0
		if ok {
			if n, err := strconv.ParseInt(s[:slash], 10, 32); err != nil || n <= 0 {
				ok = false
			} else if _, err := parseTimeSetting(key, s[slash+1:]); err != nil {
				ok = false
			}
		}
		if !ok {
			return javaIllegalArgument("parameter must contain a positive integer and a timevalue, i.e. 10/1m, but was [" + s + "]")
		}
	}
	return nil
}

// index settings on create and update -------------------------------------

// dependencies of settings on other settings (missing required setting).
var settingDependencies = map[string]string{
	"index.codec.compression_level":    "index.codec",
	"index.knn.derived_source.enabled": "index.knn",
}

// validateCreateIndexSettings validates the settings of a create index
// request (or template) in key order: unknown settings, values, setting
// dependencies and finally private settings.
func validateCreateIndexSettings(flat map[string]any, forbidPrivate bool) error {
	keys := sortedFlatKeys(flat)
	for _, key := range keys {
		d, ok := lookupIndexSetting(key)
		if !ok {
			return errUnknownIndexSetting(key)
		}
		if err := validateSetting(key, d, flat[key]); err != nil {
			return err
		}
	}
	for _, key := range keys {
		if dep, ok := settingDependencies[key]; ok && flat[key] != nil {
			if _, present := flat[dep]; !present {
				return errSettings("missing required setting [" + dep + "] for setting [" + key + "]")
			}
		}
	}
	if forbidPrivate {
		var msgs []string
		for _, key := range keys {
			if d, _ := lookupIndexSetting(key); d.private() && flat[key] != nil {
				msgs = append(msgs, fmt.Sprintf("%d: private index setting [%s] can not be set explicitly;", len(msgs)+1, key))
			}
		}
		if len(msgs) > 0 {
			return &Error{Status: http.StatusBadRequest, Type: "validation_exception", Reason: "Validation Failed: " + strings.Join(msgs, "")}
		}
	}
	return nil
}

// validateIndexSettingRelations checks settings that depend on each other
// once the final settings of an index are known.
func validateIndexSettingRelations(flat map[string]any, mapping *Mapping) error {
	shards := int64(1)
	if s, ok := flat["index.number_of_shards"].(string); ok {
		if n, err := strconv.ParseInt(s, 10, 32); err == nil {
			shards = n
		}
	}
	if s, ok := flat["index.number_of_routing_shards"].(string); ok {
		if routing, err := strconv.ParseInt(s, 10, 32); err == nil {
			if routing < shards {
				return javaIllegalArgument(fmt.Sprintf("index.number_of_routing_shards [%d] must be >= index.number_of_shards [%d]", routing, shards))
			}
			if routing > shards {
				factor := routing / shards
				if factor*shards != routing || factor <= 1 {
					return javaIllegalArgument(fmt.Sprintf("the number of source shards [%d] must be a factor of [%d]", shards, routing))
				}
			}
		}
	}
	if mapping != nil {
		if fields, ok := flat["index.sort.field"]; ok && fields != nil {
			var names []string
			if l, isList := fields.([]any); isList {
				for _, f := range l {
					names = append(names, scalarSetting(f))
				}
			} else {
				names = strings.Split(scalarSetting(fields), ",")
			}
			for _, name := range names {
				if _, _, ok := mapping.resolve(name); !ok {
					return javaIllegalArgument("unknown index sort field:[" + name + "]")
				}
			}
		}
	}
	return nil
}

func sortedFlatKeys(flat map[string]any) []string {
	keys := make([]string, 0, len(flat))
	for k := range flat {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// indexSettingDefaults returns the default values of the index settings that
// are not set explicitly (include_defaults), filtered by names.
func indexSettingDefaults(flat map[string]any, names []string) map[string]any {
	out := map[string]any{}
	for key, d := range indexSettingDefs {
		if _, set := flat[key]; set || d.private() || d.def == nil {
			continue
		}
		if s, ok := d.def.(string); ok && s == "" && (key == "index.version.created" || key == "index.provided_name") {
			continue
		}
		if !settingNamesMatch(names, key) {
			continue
		}
		def := d.def
		switch key {
		case "index.max_rescore_window":
			if v, ok := flat["index.max_result_window"]; ok {
				def = v
			}
		case "index.number_of_routing_shards":
			if v, ok := flat["index.number_of_shards"]; ok {
				def = v
			}
		}
		out[key] = def
	}
	return out
}

// clusterSettingDefaults returns the defaults of dynamic cluster settings.
func clusterSettingDefaults(set map[string]bool) map[string]any {
	out := map[string]any{}
	for key, d := range clusterSettingDefs {
		if set[key] || d.def == nil {
			continue
		}
		out[key] = d.def
	}
	return out
}
