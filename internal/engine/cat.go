package engine

import (
	"math/bits"
	"sort"
	"strconv"
	"time"
	"unicode/utf16"
)

// CatShard is one primary shard of an index as the cat APIs report it.
type CatShard struct {
	// Docs counts Lucene documents: every root document plus its nested
	// objects, like docs.count of _cat/indices and _cat/shards.
	Docs int
	// Bytes is a deterministic store size estimate: catEmptyShardBytes for
	// the commit files of an empty shard plus the _source bytes of its
	// documents. Real OpenSearch sizes depend on Lucene segment files.
	Bytes int64
	// MaxSeqNo approximates the shard's max sequence number: the index
	// counter for single-shard indices (exact), otherwise the live document
	// count minus one (exact for shards that only saw index operations,
	// since osmem numbers operations per index rather than per shard).
	MaxSeqNo int64
}

// CatIndex is a snapshot of one index for the cat APIs.
type CatIndex struct {
	Name     string
	UUID     string
	Created  time.Time
	Replicas int
	Hidden   bool
	Closed   bool
	// Shards holds the primaries in shard order; documents are assigned
	// with OpenSearch's routing (Murmur3 of the _id).
	Shards []CatShard
}

// catEmptyShardBytes is the store size OpenSearch reports for an empty
// shard (segments_N and write.lock).
const catEmptyShardBytes = 208

// CatIndices resolves an index expression the way the cat APIs do and
// returns the matched indices sorted by name.
func (c *Cluster) CatIndices(expr string, p Params) ([]CatIndex, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	indices, err := c.resolveIndices(expr, p, strictExpandOptions)
	if err != nil {
		return nil, err
	}
	out := make([]CatIndex, 0, len(indices))
	for _, ix := range indices {
		out = append(out, catIndexInfo(ix))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// CatAllIndices returns every index, hidden ones included, sorted by name
// (for cluster wide tables such as _cat/allocation).
func (c *Cluster) CatAllIndices() []CatIndex {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]CatIndex, 0, len(c.indices))
	for _, name := range c.sortedIndexNames() {
		out = append(out, catIndexInfo(c.indices[name]))
	}
	return out
}

func catIndexInfo(ix *Index) CatIndex {
	settings := getMap(ix.Settings, "index")
	shards := getInt(settings, "number_of_shards", 1)
	if shards < 1 {
		shards = 1
	}
	info := CatIndex{
		Name:     ix.Name,
		UUID:     ix.UUID,
		Created:  ix.Created,
		Replicas: getInt(settings, "number_of_replicas", 1),
		Hidden:   getBool(settings, "hidden", false),
		Closed:   catIndexClosed(ix),
		Shards:   make([]CatShard, shards),
	}
	for i := range info.Shards {
		info.Shards[i] = CatShard{Bytes: catEmptyShardBytes, MaxSeqNo: -1}
	}
	routingShards := catRoutingShards(settings, shards)
	for id, d := range ix.docs {
		s := &info.Shards[shardForRouting(id, shards, routingShards)]
		s.Docs += 1 + len(ix.children[id])
		s.Bytes += int64(len(d.Raw))
		s.MaxSeqNo++
	}
	if shards == 1 {
		info.Shards[0].MaxSeqNo = ix.seqNo
	}
	return info
}

// catIndexClosed reports whether an index is closed: closed rows render
// status close, health red and no statistics.
func catIndexClosed(ix *Index) bool { return ix.stateClosed }

// catRoutingShards returns index.number_of_routing_shards, defaulting like
// MetadataCreateIndexService.calculateNumRoutingShards (indices created on
// or after 7.0 can be split up to 1024 shards).
func catRoutingShards(settings M, shards int) int {
	if v, ok := settings["number_of_routing_shards"]; ok {
		if n, err := strconv.Atoi(stringOf(v)); err == nil && n >= shards && n%shards == 0 {
			return n
		}
	}
	splits := 10 - bits.Len(uint(shards-1))
	if splits <= 0 {
		return shards
	}
	return shards << splits
}

// shardForRouting implements OperationRouting.generateShardId for indices
// without a routing partition: floorMod(murmur3(routing), routingShards) /
// routingFactor.
func shardForRouting(routing string, shards, routingShards int) int {
	hash := int64(murmur3Routing(routing))
	mod := hash % int64(routingShards)
	if mod < 0 {
		mod += int64(routingShards)
	}
	return int(mod) / (routingShards / shards)
}

// murmur3Routing is Murmur3HashFunction.hash: murmurhash3_x86_32 (seed 0)
// of the UTF-16 code units in little endian order.
func murmur3Routing(s string) int32 {
	units := utf16.Encode([]rune(s))
	data := make([]byte, 0, len(units)*2)
	for _, u := range units {
		data = append(data, byte(u), byte(u>>8))
	}
	const c1, c2 = 0xcc9e2d51, 0x1b873593
	var h uint32
	n := len(data) &^ 3
	for i := 0; i < n; i += 4 {
		k := uint32(data[i]) | uint32(data[i+1])<<8 | uint32(data[i+2])<<16 | uint32(data[i+3])<<24
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
		k = uint32(data[n+2]) << 16
		fallthrough
	case 2:
		k |= uint32(data[n+1]) << 8
		fallthrough
	case 1:
		k |= uint32(data[n])
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
