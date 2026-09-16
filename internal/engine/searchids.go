package engine

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/bits"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Shard routing, time values, request validation and the scroll / point in
// time id formats of OpenSearch.

// murmur3x86_32 is Lucene's StringHelper.murmurhash3_x86_32.
func murmur3x86_32(data []byte, seed uint32) int32 {
	const c1, c2 = 0xcc9e2d51, 0x1b873593
	h := seed
	n := len(data) / 4 * 4
	for i := 0; i < n; i += 4 {
		k := binary.LittleEndian.Uint32(data[i:])
		k *= c1
		k = bits.RotateLeft32(k, 15)
		k *= c2
		h ^= k
		h = bits.RotateLeft32(h, 13)
		h = h*5 + 0xe6546b64
	}
	var k uint32
	switch len(data) & 3 {
	case 3:
		k ^= uint32(data[n+2]) << 16
		fallthrough
	case 2:
		k ^= uint32(data[n+1]) << 8
		fallthrough
	case 1:
		k ^= uint32(data[n])
		k *= c1
		k = bits.RotateLeft32(k, 15)
		k *= c2
		h ^= k
	}
	h ^= uint32(len(data))
	h ^= h >> 16
	h *= 0x85ebca6b
	h ^= h >> 13
	h *= 0xc2b2ae35
	h ^= h >> 16
	return int32(h)
}

func floorMod(a, n int64) int64 {
	m := a % n
	if m < 0 {
		m += n
	}
	return m
}

// routingShardCounts returns index.number_of_routing_shards and the routing
// factor (IndexMetadata.getRoutingNumShards / getRoutingFactor).
func routingShardCounts(ix *Index) (int, int) {
	shards := indexShardCount(ix)
	routing := getInt(getMap(ix.Settings, "index"), "number_of_routing_shards", 0)
	if routing <= 0 {
		log2 := 0
		if shards > 1 {
			log2 = bits.Len(uint(shards - 1))
		}
		splits := 10 - log2
		if splits < 1 {
			splits = 1
		}
		routing = shards << splits
	}
	factor := routing / shards
	if factor < 1 {
		factor = 1
	}
	return routing, factor
}

// shardOf is OperationRouting.generateShardId for a document routed by its id.
func shardOf(ix *Index, id string) int {
	shards := indexShardCount(ix)
	if shards == 1 {
		return 0
	}
	buf := make([]byte, 0, len(id)*2)
	for _, u := range utf16Units(id) {
		buf = append(buf, byte(u), byte(u>>8))
	}
	routing, factor := routingShardCounts(ix)
	return int(floorMod(int64(murmur3x86_32(buf, 0)), int64(routing))) / factor
}

func utf16Units(s string) []uint16 {
	out := make([]uint16, 0, len(s))
	for _, r := range s {
		if r >= 0x10000 {
			r -= 0x10000
			out = append(out, uint16(0xd800+(r>>10)), uint16(0xdc00+(r&0x3ff)))
			continue
		}
		out = append(out, uint16(r))
	}
	return out
}

// encodeDocID is Uid.encodeId, the indexed form of _id.
func encodeDocID(id string) []byte {
	numeric := id != ""
	for i := 0; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			numeric = false
			break
		}
	}
	if numeric {
		b := make([]byte, 1, 1+(len(id)+1)/2)
		b[0] = 0xfe
		for i := 0; i < len(id); i += 2 {
			b1 := id[i] - '0'
			b2 := byte(0x0f)
			if i+1 < len(id) {
				b2 = id[i+1] - '0'
			}
			b = append(b, b1<<4|b2)
		}
		return b
	}
	if isURLBase64WithoutPadding(id) {
		if b, err := base64.RawURLEncoding.DecodeString(id); err == nil && len(b) > 0 {
			if b[0] >= 0xfd {
				b = append([]byte{0xfd}, b...)
			}
			return b
		}
	}
	return append([]byte{0xff}, id...)
}

func isURLBase64WithoutPadding(id string) bool {
	switch len(id) & 3 {
	case 1:
		return false
	case 2:
		if !strings.ContainsRune("AQgw", rune(id[len(id)-1])) {
			return false
		}
	case 3:
		if !strings.ContainsRune("AEIMQUYcgkosw048", rune(id[len(id)-1])) {
			return false
		}
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '-' || c == '_') {
			return false
		}
	}
	return len(id) > 0
}

// bitMix64 is BitMixer.mix64, the hash of numeric slice values.
func bitMix64(z uint64) uint64 {
	z = (z ^ (z >> 32)) * 0x4cd6944c5cc20b6d
	z = (z ^ (z >> 29)) * 0xfc12c5b19d3259e9
	return z ^ (z >> 32)
}

// time values -------------------------------------------------------------

// parseTimeValue is TimeValue.parseTimeValue.
func parseTimeValue(value, setting string) (time.Duration, error) {
	normalized := strings.TrimSpace(strings.ToLower(value))
	unit := func(suffix string, d time.Duration) (time.Duration, error) {
		s := strings.TrimSpace(normalized[:len(normalized)-len(suffix)])
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			if _, ferr := strconv.ParseFloat(s, 64); ferr == nil {
				return 0, &Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "failed to parse [" + value + "], fractional time values are not supported",
					Cause: &Error{Type: "number_format_exception", Reason: "For input string: \"" + s + "\""}}
			}
			return 0, &Error{Status: http.StatusBadRequest, Type: "parse_exception", Reason: "failed to parse [" + value + "]",
				Cause: &Error{Type: "number_format_exception", Reason: "For input string: \"" + s + "\""}}
		}
		if n < -1 {
			return 0, errIllegalArgument("failed to parse setting [%s] with value [%s] as a time value: negative durations are not supported", setting, value)
		}
		return time.Duration(n) * d, nil
	}
	switch {
	case strings.HasSuffix(normalized, "nanos"):
		return unit("nanos", time.Nanosecond)
	case strings.HasSuffix(normalized, "micros"):
		return unit("micros", time.Microsecond)
	case strings.HasSuffix(normalized, "ms"):
		return unit("ms", time.Millisecond)
	case strings.HasSuffix(normalized, "s"):
		return unit("s", time.Second)
	case strings.HasSuffix(value, "m"):
		return unit("m", time.Minute)
	case strings.HasSuffix(normalized, "h"):
		return unit("h", time.Hour)
	case strings.HasSuffix(normalized, "d"):
		return unit("d", 24*time.Hour)
	case strings.Trim(normalized, "0") == "-1" && strings.HasPrefix(normalized, "-"):
		return -1, nil
	case normalized != "" && strings.Trim(normalized, "0") == "":
		return 0, nil
	}
	return 0, errIllegalArgument("failed to parse setting [%s] with value [%s] as a time value: unit is missing or unrecognized", setting, value)
}

// formatTimeValue is TimeValue.toString.
func formatTimeValue(d time.Duration) string {
	if d < 0 {
		return strconv.FormatInt(int64(d/time.Millisecond), 10)
	}
	if d == 0 {
		return "0s"
	}
	value, suffix := float64(d), "nanos"
	switch {
	case d >= 24*time.Hour:
		value, suffix = float64(d)/float64(24*time.Hour), "d"
	case d >= time.Hour:
		value, suffix = float64(d)/float64(time.Hour), "h"
	case d >= time.Minute:
		value, suffix = float64(d)/float64(time.Minute), "m"
	case d >= time.Second:
		value, suffix = float64(d)/float64(time.Second), "s"
	case d >= time.Millisecond:
		value, suffix = float64(d)/float64(time.Millisecond), "ms"
	case d >= time.Microsecond:
		value, suffix = float64(d)/float64(time.Microsecond), "micros"
	}
	text := strconv.FormatFloat(value, 'f', -1, 64)
	if i := strings.IndexByte(text, '.'); i >= 0 {
		if text[i+1] == '0' {
			text = text[:i]
		} else {
			text = text[:i+2]
		}
	}
	return text + suffix
}

// maxKeepAlive reads search.max_keep_alive or point_in_time.max_keep_alive.
func (c *Cluster) maxKeepAlive(setting string) time.Duration {
	for _, scope := range []string{"transient", "persistent"} {
		if m, ok := c.clusterSettings[scope].(M); ok {
			if v, ok := m[setting]; ok {
				if d, err := parseTimeValue(fmt.Sprint(v), setting); err == nil && d > 0 {
					return d
				}
			}
		}
	}
	return 24 * time.Hour
}

func errKeepAliveTooLarge(keepAlive, max time.Duration, setting string) *Error {
	return errIllegalArgument("Keep alive for request (%s) is too large. It must be less than (%s). This limit can be set by changing the [%s] cluster level setting.",
		formatTimeValue(keepAlive.Truncate(time.Millisecond)), formatTimeValue(max), setting)
}

// validation --------------------------------------------------------------

// validationErrors accumulates ActionRequestValidationException messages.
type validationErrors []string

func (v *validationErrors) add(format string, args ...any) {
	*v = append(*v, fmt.Sprintf(format, args...))
}

func (v validationErrors) err() error {
	if len(v) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("Validation Failed: ")
	for i, msg := range v {
		fmt.Fprintf(&sb, "%d: %s;", i+1, msg)
	}
	return &Error{Status: http.StatusBadRequest, Type: "action_request_validation_exception", Reason: sb.String()}
}

// parseIntParam is RestRequest.paramAsInt.
func parseIntParam(p Params, name string) (int, bool, error) {
	v, ok := p[name]
	if !ok {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, true, &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "Failed to parse int parameter [" + name + "] with value [" + v + "]",
			Cause: &Error{Type: "number_format_exception", Reason: "For input string: \"" + v + "\""}}
	}
	return int(n), true, nil
}

// stream ids ----------------------------------------------------------------

// osmemNodeID is the node id written into scroll and point in time ids.
const osmemNodeID = "osmem-node"

// osmemContextUUID is the reader context session written into ids.
const osmemContextUUID = "osmemReaderContextUUID"

// pitVersionID is the wire id of OpenSearch 3.8.0 written into PIT ids.
const pitVersionID = 137297827

type streamOut struct{ b []byte }

func (o *streamOut) vint(n int) {
	u := uint32(n)
	for u >= 0x80 {
		o.b = append(o.b, byte(u)|0x80)
		u >>= 7
	}
	o.b = append(o.b, byte(u))
}

func (o *streamOut) str(s string) {
	o.vint(utf8.RuneCountInString(s))
	o.b = append(o.b, s...)
}

func (o *streamOut) long(n int64) {
	o.b = binary.BigEndian.AppendUint64(o.b, uint64(n))
}

// streamIn reads the StreamInput encoding; eof reports a read past the end
// the way the concrete stream does.
type streamIn struct {
	b   []byte
	pos int
	eof func(want, available int) *Error
}

func (in *streamIn) byte1() (byte, error) {
	if in.pos >= len(in.b) {
		return 0, in.eof(1, 0)
	}
	b := in.b[in.pos]
	in.pos++
	return b, nil
}

func (in *streamIn) vint() (int, error) {
	var n uint32
	for shift := 0; shift <= 28; shift += 7 {
		b, err := in.byte1()
		if err != nil {
			return 0, err
		}
		if shift == 28 && b&0x80 != 0 {
			return 0, &Error{Type: "i_o_exception", Reason: fmt.Sprintf("Invalid vInt ((%x & 0x7f) << 28) | %x", b, n)}
		}
		n |= uint32(b&0x7f) << shift
		if b&0x80 == 0 {
			break
		}
	}
	return int(int32(n)), nil
}

func (in *streamIn) str(sizeErr func(want, available int) *Error) (string, error) {
	n, err := in.vint()
	if err != nil {
		return "", err
	}
	if n < 0 {
		return "", &Error{Type: "negative_array_size_exception", Reason: "array size must be positive but was: " + strconv.Itoa(n)}
	}
	if n > len(in.b)-in.pos {
		return "", sizeErr(n, len(in.b)-in.pos)
	}
	start := in.pos
	for i := 0; i < n; i++ {
		if in.pos >= len(in.b) {
			return "", in.eof(1, 0)
		}
		_, size := utf8.DecodeRune(in.b[in.pos:])
		in.pos += size
	}
	return string(in.b[start:in.pos]), nil
}

func (in *streamIn) long() (int64, error) {
	if len(in.b)-in.pos < 8 {
		in.pos = len(in.b)
		return 0, in.eof(8, 0)
	}
	n := int64(binary.BigEndian.Uint64(in.b[in.pos:]))
	in.pos += 8
	return n, nil
}

// decodeBase64URL is Base64.getUrlDecoder().decode.
func decodeBase64URL(s string) ([]byte, *Error) {
	trimmed := strings.TrimRight(s, "=")
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '-' || c == '_') {
			return nil, &Error{Type: "illegal_argument_exception", Reason: "Illegal base64 character " + strconv.FormatInt(int64(int8(c)), 16)}
		}
	}
	if len(trimmed)%4 == 1 {
		return nil, &Error{Type: "illegal_argument_exception", Reason: "Last unit does not have enough valid bits"}
	}
	b, err := base64.RawURLEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, &Error{Type: "illegal_argument_exception", Reason: err.Error()}
	}
	return b, nil
}

// shardContext is one shard of a scroll or point in time.
type shardContext struct {
	index string
	uuid  string
	shard int
}

func targetShardContexts(ts []target) []shardContext {
	var out []shardContext
	for _, t := range ts {
		for s := 0; s < indexShardCount(t.ix); s++ {
			out = append(out, shardContext{index: t.ix.Name, uuid: t.ix.UUID, shard: s})
		}
	}
	return out
}

// encodeScrollID writes TransportSearchHelper.buildScrollId.
func encodeScrollID(ctx int64, shards []shardContext) string {
	o := &streamOut{}
	o.str("include_context_uuid")
	if len(shards) == 1 {
		o.str("queryAndFetch")
	} else {
		o.str("queryThenFetch")
	}
	o.vint(len(shards))
	for i := range shards {
		o.str(osmemContextUUID)
		o.long(ctx + int64(i))
		o.str(osmemNodeID)
	}
	o.vint(len(shards))
	for _, s := range shards {
		o.b = append(o.b, 1)
		o.str(s.index)
	}
	return base64.URLEncoding.EncodeToString(o.b)
}

func errCannotParseScrollID(cause *Error) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "Cannot parse scroll id", Cause: cause}
}

// decodeScrollID parses a scroll id (TransportSearchHelper.parseScrollId)
// and returns the first context id and its node.
func decodeScrollID(id string) (int64, string, error) {
	raw, derr := decodeBase64URL(id)
	if derr != nil {
		return 0, "", errCannotParseScrollID(derr)
	}
	in := &streamIn{b: raw, eof: func(int, int) *Error {
		return &Error{Type: "array_index_out_of_bounds_exception", Reason: fmt.Sprintf("Index %d out of bounds for length %d", len(raw), len(raw))}
	}}
	sizeErr := func(want, available int) *Error {
		return &Error{Type: "e_o_f_exception", Reason: fmt.Sprintf("attempting to read %d bytes but only %d bytes are available", want, available)}
	}
	fail := func(err error) (int64, string, error) {
		if e, ok := err.(*Error); ok {
			return 0, "", errCannotParseScrollID(e)
		}
		return 0, "", errCannotParseScrollID(&Error{Type: "exception", Reason: err.Error()})
	}
	first, err := in.str(sizeErr)
	if err != nil {
		return fail(err)
	}
	withUUID := first == "include_context_uuid"
	if withUUID {
		if _, err := in.str(sizeErr); err != nil {
			return fail(err)
		}
	}
	n, err := in.vint()
	if err != nil {
		return fail(err)
	}
	if n < 0 {
		return fail(&Error{Type: "negative_array_size_exception", Reason: "-" + strconv.Itoa(-n)})
	}
	var ctx int64
	node := ""
	for i := 0; i < n; i++ {
		if withUUID {
			if _, err := in.str(sizeErr); err != nil {
				return fail(err)
			}
		}
		cid, err := in.long()
		if err != nil {
			return fail(err)
		}
		target, err := in.str(sizeErr)
		if err != nil {
			return fail(err)
		}
		if i == 0 {
			ctx, node = cid, target
			if j := strings.IndexByte(target, ':'); j >= 0 {
				node = target[j+1:]
			}
		}
	}
	if in.pos < len(in.b) {
		count, err := in.vint()
		if err != nil {
			return fail(err)
		}
		for i := 0; i < count; i++ {
			present, err := in.byte1()
			if err != nil {
				return fail(err)
			}
			if present != 0 {
				if _, err := in.str(sizeErr); err != nil {
					return fail(err)
				}
			}
		}
	}
	if in.pos != len(in.b) {
		return fail(errIllegalArgument("Not all bytes were read"))
	}
	return ctx, node, nil
}

// encodePITID writes a point in time id (SearchContextId.encode).
func encodePITID(ctx int64, shards []shardContext) string {
	o := &streamOut{}
	o.vint(pitVersionID)
	o.vint(len(shards))
	for i, s := range shards {
		o.str(s.index)
		o.str(s.uuid)
		o.vint(s.shard)
		o.str(osmemNodeID)
		o.b = append(o.b, 0)
		o.str(osmemContextUUID)
		o.long(ctx + int64(i))
	}
	o.vint(0)
	return base64.URLEncoding.EncodeToString(o.b)
}

func errInvalidPITID(id string, cause *Error) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "illegal_argument_exception", Reason: "invalid id: [" + id + "]", Cause: cause}
}

// decodePITID parses a point in time id; it returns the first context id
// and the shards, or the error OpenSearch 3.8 reports for a malformed id.
func decodePITID(id string) (int64, []shardContext, error) {
	raw, derr := decodeBase64URL(id)
	if derr != nil {
		return 0, nil, errInvalidPITID(id, derr)
	}
	in := &streamIn{b: raw, eof: func(int, int) *Error { return &Error{Type: "e_o_f_exception", nullReason: true} }}
	sizeErr := func(want, available int) *Error {
		return &Error{Type: "e_o_f_exception", Reason: fmt.Sprintf("tried to read: %d bytes but only %d remaining", want, available)}
	}
	fail := func(err error) (int64, []shardContext, error) {
		if e, ok := err.(*Error); ok {
			if e.Type == "negative_array_size_exception" {
				// not an IOException: it escapes the id decoding
				return 0, nil, &Error{Status: http.StatusInternalServerError, Type: e.Type, Reason: e.Reason}
			}
			return 0, nil, errInvalidPITID(id, e)
		}
		return 0, nil, errInvalidPITID(id, &Error{Type: "exception", Reason: err.Error()})
	}
	version, err := in.vint()
	if err != nil {
		return fail(err)
	}
	if version != 0 && version&0x08000000 == 0 {
		major, minor, revision := version/1000000%100, version/10000%100, version/100%100
		return 0, nil, &Error{Status: http.StatusInternalServerError, Type: "unsupported_version_exception",
			Reason: fmt.Sprintf("Unsupported version [ES %d.%d.%d]", major, minor, revision)}
	}
	n, err := in.vint()
	if err != nil {
		return fail(err)
	}
	if n < 0 {
		return fail(&Error{Type: "negative_array_size_exception", Reason: "array size must be positive but was: " + strconv.Itoa(n)})
	}
	var ctx int64
	// n is client-supplied: cap the capacity hint by the bytes remaining
	// (every shard context takes at least a few bytes) so a huge count
	// fails on a short read instead of allocating first
	shards := make([]shardContext, 0, min(n, len(in.b)-in.pos))
	for i := 0; i < n; i++ {
		var s shardContext
		if s.index, err = in.str(sizeErr); err != nil {
			return fail(err)
		}
		if s.uuid, err = in.str(sizeErr); err != nil {
			return fail(err)
		}
		if s.shard, err = in.vint(); err != nil {
			return fail(err)
		}
		if _, err = in.str(sizeErr); err != nil {
			return fail(err)
		}
		alias, err := in.byte1()
		if err != nil {
			return fail(err)
		}
		if alias != 0 {
			if _, err = in.str(sizeErr); err != nil {
				return fail(err)
			}
		}
		if _, err = in.str(sizeErr); err != nil {
			return fail(err)
		}
		cid, err := in.long()
		if err != nil {
			return fail(err)
		}
		if i == 0 {
			ctx = cid
		}
		shards = append(shards, s)
	}
	if _, err := in.vint(); err != nil {
		return fail(err)
	}
	return ctx, shards, nil
}
