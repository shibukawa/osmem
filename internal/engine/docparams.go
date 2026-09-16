package engine

import (
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// OpenSearch's sentinel values for versions and sequence numbers.
const (
	versionMatchAny     int64 = -3 // Versions.MATCH_ANY
	versionMatchDeleted int64 = -4 // Versions.MATCH_DELETED
	versionNotFound     int64 = -1 // Versions.NOT_FOUND
	unassignedSeqNo     int64 = -2 // SequenceNumbers.UNASSIGNED_SEQ_NO
	unassignedTerm      int64 = 0  // SequenceNumbers.UNASSIGNED_PRIMARY_TERM
)

func numberFormatCause(value string) *Error {
	return &Error{Type: "number_format_exception", Reason: "For input string: \"" + value + "\""}
}

// paramLong is RestRequest.paramAsLong.
func paramLong(p Params, name string) (int64, bool, error) {
	if !p.Has(name) {
		return 0, false, nil
	}
	v := p.Get(name)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, true, &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception",
			Reason: "Failed to parse long parameter [" + name + "] with value [" + v + "]", Cause: numberFormatCause(v)}
	}
	return n, true, nil
}

// paramInt is RestRequest.paramAsInt.
func paramInt(p Params, name string) (int, bool, error) {
	if !p.Has(name) {
		return 0, false, nil
	}
	v := p.Get(name)
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, true, &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception",
			Reason: "Failed to parse int parameter [" + name + "] with value [" + v + "]", Cause: numberFormatCause(v)}
	}
	return int(n), true, nil
}

func errBooleanValue(v string) *Error {
	return errIllegalArgument("Failed to parse value [%s] as only [true] or [false] are allowed.", v)
}

// paramBool is RestRequest.paramAsBoolean: a bare flag is true, otherwise only
// "true" and "false" are accepted.
func paramBool(p Params, name string, def bool) (bool, error) {
	if !p.Has(name) {
		return def, nil
	}
	switch v := p.Get(name); v {
	case "", "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return def, errBooleanValue(v)
	}
}

// timeValue is a parsed TimeValue; text is its string representation.
type timeValue struct {
	text string
}

var numericRe = regexp.MustCompile(`^-?\d+$`)

// parseTimeValue is TimeValue.parseTimeValue.
func parseTimeText(value, setting string) (timeValue, error) {
	normalized := strings.TrimSpace(strings.ToLower(value))
	unitErr := func() error {
		return errIllegalArgument("failed to parse setting [%s] with value [%s] as a time value: unit is missing or unrecognized", setting, value)
	}
	for _, suffix := range []string{"nanos", "micros", "ms", "s", "m", "h", "d"} {
		if suffix == "m" {
			if !strings.HasSuffix(value, "m") {
				continue
			}
		} else if !strings.HasSuffix(normalized, suffix) {
			continue
		}
		num := strings.TrimSpace(strings.TrimSuffix(normalized, suffix))
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			if _, ferr := strconv.ParseFloat(num, 64); ferr == nil && num != "" {
				return timeValue{}, &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception",
					Reason: "failed to parse [" + value + "], fractional time values are not supported", Cause: numberFormatCause(num)}
			}
			return timeValue{}, &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception",
				Reason: "failed to parse [" + value + "]", Cause: numberFormatCause(num)}
		}
		if n < -1 {
			return timeValue{}, errIllegalArgument("failed to parse setting [%s] with value [%s] as a time value: negative durations are not supported", setting, value)
		}
		return timeValue{text: strconv.FormatInt(n, 10) + suffix}, nil
	}
	if regexp.MustCompile(`^-0*1$`).MatchString(normalized) {
		return timeValue{text: "-1"}, nil
	}
	if regexp.MustCompile(`^0+$`).MatchString(normalized) {
		return timeValue{text: "0s"}, nil
	}
	return timeValue{}, unitErr()
}

// paramTime parses a time parameter; def is used when it is absent.
func paramTime(p Params, name, def string) (timeValue, error) {
	if !p.Has(name) {
		return timeValue{text: def}, nil
	}
	return parseTimeText(p.Get(name), name)
}

// checkRefreshParam is WriteRequest.RefreshPolicy.parse.
func checkRefreshParam(p Params) error {
	if !p.Has("refresh") {
		return nil
	}
	switch v := p.Get("refresh"); v {
	case "", "true", "false", "wait_for":
		return nil
	default:
		return errIllegalArgument("Unknown value for refresh: [%s].", v)
	}
}

// activeShardCount is a parsed wait_for_active_shards value; all is ALL,
// otherwise n copies are required (-2 is the default).
type activeShardCount struct {
	set bool
	all bool
	n   int
}

// parseActiveShardCount is ActiveShardCount.parseString.
func parseShardCount(s string) (activeShardCount, error) {
	if s == "all" {
		return activeShardCount{set: true, all: true}, nil
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return activeShardCount{}, &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception",
			Reason: "cannot parse ActiveShardCount[" + s + "]", Cause: numberFormatCause(s)}
	}
	if n < 0 {
		return activeShardCount{}, errIllegalArgument("shard count cannot be a negative value")
	}
	return activeShardCount{set: true, n: int(n)}, nil
}

func paramActiveShards(p Params) (activeShardCount, error) {
	if !p.Has("wait_for_active_shards") {
		return activeShardCount{}, nil
	}
	return parseShardCount(p.Get("wait_for_active_shards"))
}

// checkActiveShards fails a write the way the replication layer does when
// the required number of shard copies is not active: this single-node
// cluster only ever has the primary.
func checkActiveShards(ix *Index, wait activeShardCount, timeout timeValue) error {
	if !wait.set {
		return nil
	}
	copies := 1
	if ix != nil {
		replicas := getInt(getMap(ix.Settings, "index"), "number_of_replicas", 1)
		if replicas > 0 {
			copies += replicas
		}
	}
	needed, label := wait.n, strconv.Itoa(wait.n)
	if wait.all {
		needed, label = copies, "ALL"
	}
	if needed <= 1 {
		return nil
	}
	name := ""
	if ix != nil {
		name = ix.Name
	}
	return &Error{Status: http.StatusServiceUnavailable, Type: "unavailable_shards_exception",
		Reason: "[" + name + "][0] Not enough active copies to meet shard count of [" + label + "] (have 1, needed " + strconv.Itoa(needed) + "). Timeout: [" + timeout.text + "]"}
}

// parseVersionType is VersionType.fromString.
func parseVersionType(s string) (string, error) {
	switch s {
	case "internal", "external", "external_gt", "external_gte":
		return s, nil
	}
	return "", errIllegalArgument("No version type match [%s]", s)
}

func isExternalVersioning(vt string) bool {
	return vt == "external" || vt == "external_gt" || vt == "external_gte"
}

// docValidation accumulates ActionRequestValidationException messages.
type docValidation []string

func (v *docValidation) add(msg string) { *v = append(*v, msg) }

func (v docValidation) err() error {
	if len(v) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("Validation Failed: ")
	for i, m := range v {
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(": ")
		b.WriteString(m)
		b.WriteString(";")
	}
	return &Error{Status: http.StatusBadRequest, Type: "action_request_validation_exception", Reason: b.String()}
}

// validateVersionForWrites is VersionType.validateVersionForWrites.
func validateVersionForWrites(vt string, version int64) bool {
	if isExternalVersioning(vt) {
		return version >= 0
	}
	return version > 0 || version == versionMatchAny || version == versionMatchDeleted
}

// validateCASParams is DocWriteRequest.validateSeqNoBasedCASParams.
func (dp *DocParams) validateCASParams(v *docValidation) {
	vt := dp.versionType()
	version := dp.version()
	if !validateVersionForWrites(vt, version) {
		v.add("illegal version value [" + strconv.FormatInt(version, 10) + "] for version type [" + strings.ToUpper(vt) + "]")
	}
	if vt == "internal" && version != versionMatchAny && version != versionMatchDeleted {
		v.add("internal versioning can not be used for optimistic concurrency control. Please use `if_seq_no` and `if_primary_term` instead")
	}
	seqNo, term := dp.ifSeqNo(), dp.ifPrimaryTerm()
	if seqNo != unassignedSeqNo && (vt != "internal" || version != versionMatchAny) {
		v.add("compare and write operations can not use versioning")
	}
	if term == unassignedTerm && seqNo != unassignedSeqNo {
		v.add("ifSeqNo is set, but primary term is [0]")
	}
	if term != unassignedTerm && seqNo == unassignedSeqNo {
		v.add("ifSeqNo is unassigned, but primary term is [" + strconv.FormatInt(term, 10) + "]")
	}
}

// validateDocIDLength is DocWriteRequest.validateDocIdLength.
func validateDocIDLength(id string, v *docValidation) {
	if len(id) > 512 {
		v.add("id [" + id + "] is too long, must be no longer than 512 bytes but was: " + strconv.Itoa(len(id)))
	}
}

func (dp *DocParams) versionType() string {
	if dp.VersionType == "" {
		return "internal"
	}
	return dp.VersionType
}

func (dp *DocParams) version() int64 {
	if dp.Version == nil {
		return versionMatchAny
	}
	return *dp.Version
}

func (dp *DocParams) ifSeqNo() int64 {
	if dp.IfSeqNo == nil {
		return unassignedSeqNo
	}
	return *dp.IfSeqNo
}

func (dp *DocParams) ifPrimaryTerm() int64 {
	if dp.IfPrimaryTerm == nil {
		return unassignedTerm
	}
	return *dp.IfPrimaryTerm
}

// setIfSeqNo and setIfPrimaryTerm are the IndexRequest setters.
func checkSeqNo(n int64) error {
	if n < 0 && n != unassignedSeqNo {
		return errIllegalArgument("sequence numbers must be non negative. got [%d].", n)
	}
	return nil
}

func checkPrimaryTerm(n int64) error {
	if n < 0 {
		return errIllegalArgument("primary term must be non negative. got [%d]", n)
	}
	return nil
}

// validateIndexRequest is IndexRequest.validate.
func (dp *DocParams) validateIndexRequest(id string, hasID bool) error {
	var v docValidation
	dp.collectIndexValidation(id, hasID, &v)
	return v.err()
}

func (dp *DocParams) collectIndexValidation(id string, hasID bool, v *docValidation) {
	vt := dp.versionType()
	resolvedVersion := dp.version()
	if dp.OpType == "create" && resolvedVersion == versionMatchAny {
		resolvedVersion = versionMatchDeleted
	}
	if dp.OpType == "create" {
		if vt != "internal" {
			v.add("create operations only support internal versioning. use index instead")
			return
		}
		if resolvedVersion != versionMatchDeleted {
			v.add("create operations do not support explicit versions. use index instead")
			return
		}
		if dp.ifSeqNo() != unassignedSeqNo || dp.ifPrimaryTerm() != unassignedTerm {
			v.add("create operations do not support compare and set. use index instead")
			return
		}
	}
	if !hasID {
		if vt != "internal" || (resolvedVersion != versionMatchDeleted && resolvedVersion != versionMatchAny) {
			v.add("an id must be provided if version type or value are set")
		}
	}
	dp.validateCASParams(v)
	validateDocIDLength(id, v)
	if dp.Pipeline != nil && *dp.Pipeline == "" {
		v.add("pipeline cannot be an empty string")
	}
}

// validateDeleteRequest is DeleteRequest.validate.
func (dp *DocParams) validateDeleteRequest(id string) error {
	var v docValidation
	if id == "" {
		v.add("id is missing")
	}
	dp.validateCASParams(&v)
	return v.err()
}

// bigLongValue converts a JSON number or string to a long the way
// XContentParser.longValue does (fractions are truncated).
func bigLongValue(text string) (int64, error) {
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n, nil
	}
	r, ok := new(big.Rat).SetString(text)
	if !ok || strings.ContainsAny(text, "xXpP_") {
		return 0, errIllegalArgument("For input string: \"%s\"", text)
	}
	whole := new(big.Int).Quo(r.Num(), r.Denom())
	if !whole.IsInt64() {
		return 0, errIllegalArgument("Value [%s] is out of range for a long", text)
	}
	return whole.Int64(), nil
}
