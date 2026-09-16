package osmem

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The expected bodies below are responses of OpenSearch 3.8.0 for the same
// fixture (catTestCluster).

var catLong = "catfix-" + strings.Repeat("x", 150)

func catTestCluster(t *testing.T) *Cluster {
	t.Helper()
	c := New()
	mustDo(t, c, http.MethodPut, "/catfix-a", `{"settings":{"number_of_shards":2,"number_of_replicas":0},"aliases":{"catfix-al1":{"filter":{"term":{"k":"x"}}},"catfix-al2":{"is_write_index":true}}}`)
	mustDo(t, c, http.MethodPut, "/catfix-b", `{"settings":{"number_of_shards":1,"number_of_replicas":1},"aliases":{"catfix-al1":{},"catfix-al3":{"is_write_index":false}}}`)
	mustDo(t, c, http.MethodPut, "/catfix-c", `{"settings":{"number_of_shards":3,"number_of_replicas":0}}`)
	mustDo(t, c, http.MethodPut, "/_template/catfix-legacy", `{"index_patterns":["catfix-lg-*"],"order":2,"version":1}`)
	mustDo(t, c, http.MethodPut, "/_index_template/catfix-tpl", `{"index_patterns":["catfix-tp-*","catfix-tq-*"],"priority":5,"version":3}`)
	mustDo(t, c, http.MethodPut, "/_index_template/catfix-tpl2", `{"index_patterns":["catfix-tz-*"]}`)
	var bulk strings.Builder
	add := func(index, id, k string) {
		bulk.WriteString(`{"index":{"_index":"` + index + `","_id":"` + id + `"}}` + "\n" + `{"k":"` + k + `"}` + "\n")
	}
	add("catfix-a", "1", "x")
	add("catfix-a", "2", "y")
	add("catfix-a", "3", "z")
	add("catfix-b", "1", "x")
	for _, id := range []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"} {
		add("catfix-c", id, "v")
	}
	mustDo(t, c, http.MethodPost, "/_bulk?refresh=true", bulk.String())
	mustDo(t, c, http.MethodPut, "/"+catLong, `{"settings":{"number_of_shards":1,"number_of_replicas":0}}`)
	return c
}

func TestCatTablesMatchOpenSearch(t *testing.T) {
	c := catTestCluster(t)
	defer c.Close()
	cases := []struct {
		path   string
		status int
		body   string
	}{
		{"/_cat/indices/catfix-*?v&h=health,status,index,pri,rep,docs.count,docs.deleted&s=index", 200, "health status index pri rep docs.count docs.deleted\ngreen  open   catfix-a   2   0          3            0\nyellow open   catfix-b   1   1          1            0\ngreen  open   catfix-c   3   0         12            0\ngreen  open   " + catLong + "   1   0          0            0\n"},
		{"/_cat/indices/catfix-*?s=index:desc&h=index", 200, catLong + "\ncatfix-c\ncatfix-b\ncatfix-a\n"},
		{"/_cat/indices/catfix-*?format=json&h=index,pri,docs.count&s=index", 200, "[{\"index\":\"catfix-a\",\"pri\":\"2\",\"docs.count\":\"3\"},{\"index\":\"catfix-b\",\"pri\":\"1\",\"docs.count\":\"1\"},{\"index\":\"catfix-c\",\"pri\":\"3\",\"docs.count\":\"12\"},{\"index\":\"" + catLong + "\",\"pri\":\"1\",\"docs.count\":\"0\"}]"},
		{"/_cat/indices/catfix-*?format=json&pretty&h=index,health&s=index", 200, "[\n  {\n    \"index\" : \"catfix-a\",\n    \"health\" : \"green\"\n  },\n  {\n    \"index\" : \"catfix-b\",\n    \"health\" : \"yellow\"\n  },\n  {\n    \"index\" : \"catfix-c\",\n    \"health\" : \"green\"\n  },\n  {\n    \"index\" : \"" + catLong + "\",\n    \"health\" : \"green\"\n  }\n]\n"},
		{"/_cat/indices/catfix-*?format=yaml&h=index,pri&s=index", 200, "---\n- index: \"catfix-a\"\n  pri: \"2\"\n- index: \"catfix-b\"\n  pri: \"1\"\n- index: \"catfix-c\"\n  pri: \"3\"\n- index: \"" + catLong + "\"\n  pri: \"1\"\n"},
		{"/_cat/indices/catfix-*?h=index,foo&s=index", 200, "catfix-a\ncatfix-b\ncatfix-c\n" + catLong + "\n"},
		{"/_cat/indices/catfix-a?h=i,s,p,r,dc&v", 200, "i        s    p r dc\ncatfix-a open 2 0  3\n"},
		{"/_cat/indices/catfix-*?v&h=index,docs.count&s=docs.count:desc,index", 200, "index docs.count\ncatfix-c         12\ncatfix-a          3\ncatfix-b          1\n" + catLong + "          0\n"},
		{"/_cat/indices/catfix-*?s=nosuchcol", 500, "{\"error\":{\"root_cause\":[{\"type\":\"unsupported_operation_exception\",\"reason\":\"Unable to sort by unknown sort key `nosuchcol`\"}],\"type\":\"unsupported_operation_exception\",\"reason\":\"Unable to sort by unknown sort key `nosuchcol`\"},\"status\":500}"},
		{"/_cat/indices/catfix-*?health=yellow&h=index", 200, "catfix-b\n"},
		{"/_cat/indices/catfix-*?health=foo&h=index", 400, "{\"error\":{\"root_cause\":[{\"type\":\"illegal_argument_exception\",\"reason\":\"unknown cluster health status [foo]\"}],\"type\":\"illegal_argument_exception\",\"reason\":\"unknown cluster health status [foo]\"},\"status\":400}"},
		{"/_cat/indices/catfix-a?v=foo&h=index", 400, "{\"error\":{\"root_cause\":[{\"type\":\"illegal_argument_exception\",\"reason\":\"Failed to parse value [foo] as only [true] or [false] are allowed.\"}],\"type\":\"illegal_argument_exception\",\"reason\":\"Failed to parse value [foo] as only [true] or [false] are allowed.\"},\"status\":400}"},
		{"/_cat/indices/catfix-*?pri&v&h=index,pri.docs.count,docs.count&s=index", 200, "index docs.count\ncatfix-a          3\ncatfix-b          1\ncatfix-c         12\n" + catLong + "          0\n"},
		{"/_cat/indices/catfix-a,catfix-b," + catLong + "?v&h=index,health,pri&s=index", 200, "index health pri\ncatfix-a green    2\ncatfix-b yellow   1\n" + catLong + " green    1\n"},
		{"/_cat/indices/catfix-a,catfix-b?format=cbor&h=index,pri,docs.count&s=index", 200, "\x9f\xbfeindexhcatfix-acpria2jdocs.counta3\xff\xbfeindexhcatfix-bcpria1jdocs.counta1\xff\xff"},
		{"/_cat/indices/catfix-a,catfix-b?format=smile&h=index,pri,docs.count&s=index", 200, ":)\n\x05\xf8\xfa\x84indexGcatfix-a\x82pri@2\x89docs.count@3\xfb\xfa@Gcatfix-bA@1B@1\xfb\xf9"},
		{"/_cat/indices/catfix-*?help", 400, "{\"error\":{\"root_cause\":[{\"type\":\"illegal_argument_exception\",\"reason\":\"request [/_cat/indices/catfix-*] contains unrecognized parameter: [index]\"}],\"type\":\"illegal_argument_exception\",\"reason\":\"request [/_cat/indices/catfix-*] contains unrecognized parameter: [index]\"},\"status\":400}"},
		{"/_cat/indices/catfix-a?h=index&s=index&v&format=txt", 200, "index\ncatfix-a\n"},
		{"/_cat/aliases/catfix-al*?v&h=alias,index,filter,is_write_index&s=alias,index", 200, "alias      index    filter is_write_index\ncatfix-al1 catfix-a *      -\ncatfix-al1 catfix-b -      -\ncatfix-al2 catfix-a -      true\ncatfix-al3 catfix-b -      false\n"},
		{"/_cat/aliases/catfix-al*,-catfix-al3?h=alias,index&s=alias,index", 200, "catfix-al1 catfix-a\ncatfix-al1 catfix-b\ncatfix-al2 catfix-a\n"},
		{"/_cat/aliases?help", 200, "alias          | a                | alias name           \nindex          | i,idx            | index alias points to\nfilter         | f,fi             | filter               \nrouting.index  | ri,routingIndex  | index routing        \nrouting.search | rs,routingSearch | search routing       \nis_write_index | w,isWriteIndex   | write index          \n"},
		{"/_cat/count/catfix-a,catfix-b,catfix-c?v&h=count", 200, "count\n16\n"},
		{"/_cat/count?help", 200, "epoch     | t,time                  | seconds since 1970-01-01 00:00:00\ntimestamp | ts,hms,hhmmss           | time in HH:MM:SS                 \ncount     | dc,docs.count,docsCount | the document count               \n"},
		{"/_cat/templates/catfix-*?v&s=name", 200, "name          index_patterns             order version composed_of\ncatfix-legacy [catfix-lg-*]              2     1       \ncatfix-tpl    [catfix-tp-*, catfix-tq-*] 5     3       []\ncatfix-tpl2   [catfix-tz-*]              0             []\n"},
		{"/_cat/templates/catfix-*?format=json&s=name", 200, "[{\"name\":\"catfix-legacy\",\"index_patterns\":\"[catfix-lg-*]\",\"order\":\"2\",\"version\":\"1\",\"composed_of\":\"\"},{\"name\":\"catfix-tpl\",\"index_patterns\":\"[catfix-tp-*, catfix-tq-*]\",\"order\":\"5\",\"version\":\"3\",\"composed_of\":\"[]\"},{\"name\":\"catfix-tpl2\",\"index_patterns\":\"[catfix-tz-*]\",\"order\":\"0\",\"version\":null,\"composed_of\":\"[]\"}]"},
		{"/_cat/shards/catfix-*?v&h=index,shard,prirep,state,docs&s=index,shard,prirep", 200, "index shard prirep state      docs\ncatfix-a 0     p      STARTED       3\ncatfix-a 1     p      STARTED       0\ncatfix-b 0     p      STARTED       1\ncatfix-b 0     r      UNASSIGNED     \ncatfix-c 0     p      STARTED       2\ncatfix-c 1     p      STARTED       5\ncatfix-c 2     p      STARTED       5\n" + catLong + " 0     p      STARTED       0\n"},
		{"/_cat/shards/catfix-a?format=json&h=index,shard,prirep,state,docs&s=shard", 200, "[{\"index\":\"catfix-a\",\"shard\":\"0\",\"prirep\":\"p\",\"state\":\"STARTED\",\"docs\":\"3\"},{\"index\":\"catfix-a\",\"shard\":\"1\",\"prirep\":\"p\",\"state\":\"STARTED\",\"docs\":\"0\"}]"},
		{"/_cat/segments/catfix-*?v&h=index,shard,prirep,segment,generation,docs.count,docs.deleted&s=index,shard", 200, "index    shard prirep segment generation docs.count docs.deleted\ncatfix-a 0     p      _0               0          3            0\ncatfix-b 0     p      _0               0          1            0\ncatfix-c 0     p      _0               0          2            0\ncatfix-c 1     p      _0               0          5            0\ncatfix-c 2     p      _0               0          5            0\n"},
		{"/_cat/recovery/catfix-a?v&h=index,shard,type,stage&s=shard", 200, "index    shard type        stage\ncatfix-a 0     empty_store done\ncatfix-a 1     empty_store done\n"},
		{"/_cat/master?help", 200, "id   |   | node id    \nhost | h | host name  \nip   |   | ip address \nnode | n | node name  \n"},
		{"/_cat/health?help", 200, "epoch                      | t,time                                   | seconds since 1970-01-01 00:00:00   \ntimestamp                  | ts,hms,hhmmss                            | time in HH:MM:SS                    \ncluster                    | cl                                       | cluster name                        \nstatus                     | st                                       | health status                       \nnode.total                 | nt,nodeTotal                             | total number of nodes               \nnode.data                  | nd,nodeData                              | number of nodes that can store data \ndiscovered_cluster_manager | dcm,dm,discovered_master                 | cluster manager is discovered or not\nshards                     | t,sh,shards.total,shardsTotal            | total number of shards              \npri                        | p,shards.primary,shardsPrimary           | number of primary shards            \nrelo                       | r,shards.relocating,shardsRelocating     | number of relocating nodes          \ninit                       | i,shards.initializing,shardsInitializing | number of initializing nodes        \nunassign                   | u,shards.unassigned,shardsUnassigned     | number of unassigned shards         \npending_tasks              | pt,pendingTasks                          | number of pending tasks             \nmax_task_wait_time         | mtwt,maxTaskWaitTime                     | wait time of longest task pending   \nactive_shards_percent      | asp,activeShardsPercent                  | active number of shards in percent  \n"},
		{"/_cat/pending_tasks?v", 200, "insertOrder timeInQueue priority source\n"},
		{"/_cat/repositories?v", 200, "id type\n"},
		{"/_cat/thread_pool/search?v&h=name,type", 200, "name   type\nsearch resizable\n"},
		{"/_cat/snapshots/nosuchrepo", 404, "{\"error\":{\"root_cause\":[{\"type\":\"repository_missing_exception\",\"reason\":\"[nosuchrepo] missing\"}],\"type\":\"repository_missing_exception\",\"reason\":\"[nosuchrepo] missing\"},\"status\":404}"},
		{"/_cat", 200, "=^.^=\n/_cat/allocation\n/_cat/segment_replication\n/_cat/segment_replication/{index}\n/_cat/shards\n/_cat/shards/{index}\n/_cat/cluster_manager\n/_cat/nodes\n/_cat/tasks\n/_cat/indices\n/_cat/indices/{index}\n/_cat/segments\n/_cat/segments/{index}\n/_cat/count\n/_cat/count/{index}\n/_cat/recovery\n/_cat/recovery/{index}\n/_cat/health\n/_cat/pending_tasks\n/_cat/aliases\n/_cat/aliases/{alias}\n/_cat/thread_pool\n/_cat/thread_pool/{thread_pools}\n/_cat/plugins\n/_cat/fielddata\n/_cat/fielddata/{fields}\n/_cat/nodeattrs\n/_cat/repositories\n/_cat/snapshots/{repository}\n/_cat/templates\n"},
		{"/_cat/health?format=json&h=node.total,node.data,discovered_cluster_manager,relo,init", 200, "[{\"node.total\":\"1\",\"node.data\":\"1\",\"discovered_cluster_manager\":\"true\",\"relo\":\"0\",\"init\":\"0\"}]"},
		{"/_cat/nodes?format=json&h=node.role,node.roles,cluster_manager", 200, "[{\"node.role\":\"dimr\",\"node.roles\":\"cluster_manager,data,ingest,remote_cluster_client\",\"cluster_manager\":\"*\"}]"},
	}
	for _, tc := range cases {
		res, err := c.Do(http.MethodGet, tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != tc.status {
			t.Errorf("%s: status %d, want %d: %s", tc.path, res.StatusCode, tc.status, res.Body)
			continue
		}
		if tc.status >= 400 {
			var got, want map[string]any
			_ = json.Unmarshal(res.Body, &got)
			_ = json.Unmarshal([]byte(tc.body), &want)
			ge, _ := got["error"].(map[string]any)
			we, _ := want["error"].(map[string]any)
			if ge["type"] != we["type"] || ge["reason"] != we["reason"] {
				t.Errorf("%s:\n got %s\nwant %s", tc.path, res.Body, tc.body)
			}
			continue
		}
		if string(res.Body) != tc.body {
			t.Errorf("%s:\n got %q\nwant %q", tc.path, res.Body, tc.body)
		}
	}
}

func TestCatAcceptHeaderSelectsFormat(t *testing.T) {
	c := catTestCluster(t)
	defer c.Close()
	for accept, want := range map[string]string{
		"application/json":                "[{\"index\":\"catfix-a\",\"pri\":\"2\"}]",
		"application/json; charset=UTF-8": "[{\"index\":\"catfix-a\",\"pri\":\"2\"}]",
		"application/json, text/plain":    "catfix-a 2\n",
		"text/plain":                      "catfix-a 2\n",
	} {
		req := httptest.NewRequest(http.MethodGet, "/_cat/indices/catfix-a?h=index,pri", nil)
		req.Header.Set("Accept", accept)
		rec := httptest.NewRecorder()
		c.handler.ServeHTTP(rec, req)
		if rec.Body.String() != want {
			t.Errorf("Accept %q: got %q, want %q", accept, rec.Body.String(), want)
		}
	}
}

func TestCatYAMLSplitsLongStrings(t *testing.T) {
	c := New()
	defer c.Close()
	mustDo(t, c, http.MethodPut, "/_index_template/catprobe-long", `{"index_patterns":["catprobe-longpattern-00-*","catprobe-longpattern-01-*","catprobe-longpattern-02-*","catprobe-longpattern-03-*","catprobe-longpattern-04-*","catprobe-longpattern-05-*","catprobe-longpattern-06-*","catprobe-longpattern-07-*"],"priority":1}`)
	res, _ := c.Do(http.MethodGet, "/_cat/templates/catprobe-long?format=yaml", nil)
	// SnakeYAML folds double-quoted scalars at spaces beyond 80 columns
	want := "---\n- name: \"catprobe-long\"\n  index_patterns: \"[catprobe-longpattern-00-*, catprobe-longpattern-01-*, catprobe-longpattern-02-*,\\\n    \\ catprobe-longpattern-03-*, catprobe-longpattern-04-*, catprobe-longpattern-05-*,\\\n    \\ catprobe-longpattern-06-*, catprobe-longpattern-07-*]\"\n  order: \"1\"\n  version: null\n  composed_of: \"[]\"\n"
	if string(res.Body) != want {
		t.Fatalf("got %q\nwant %q", res.Body, want)
	}
}

func TestCatHelpAndTasks(t *testing.T) {
	c := New()
	defer c.Close()
	// a help request consumes no other parameter
	st, body := status(t, c, http.MethodGet, "/_cat/indices/logs?help&zzz=1", nil)
	if st != 400 || body["error"].(map[string]any)["reason"] != "request [/_cat/indices/logs] contains unrecognized parameters: [index], [zzz]" {
		t.Fatalf("help with parameters: %d %v", st, body)
	}
	res, _ := c.Do(http.MethodGet, "/_cat/thread_pool/search?help", nil)
	if res.StatusCode != 200 || !strings.HasPrefix(string(res.Body), "node_name         | nn  | node name                                          \n") {
		t.Fatalf("thread_pool help: %d %q", res.StatusCode, res.Body)
	}
	res, _ = c.Do(http.MethodGet, "/_cat/tasks?v&h=action,task_id,parent_task_id,type&actions=cluster:monitor/*", nil)
	lines := strings.Split(strings.TrimSuffix(string(res.Body), "\n"), "\n")
	if len(lines) != 3 || strings.Join(strings.Fields(lines[0]), " ") != "action task_id parent_task_id type" {
		t.Fatalf("tasks: %q", res.Body)
	}
	parent, child := strings.Fields(lines[1]), strings.Fields(lines[2])
	if len(parent) != 4 || len(child) != 4 || parent[0] != "cluster:monitor/tasks/lists" || parent[2] != "-" || parent[3] != "transport" ||
		child[0] != "cluster:monitor/tasks/lists[n]" || child[2] != parent[1] || child[3] != "direct" || !strings.HasPrefix(parent[1], "osmem-node:") {
		t.Fatalf("tasks: %q", res.Body)
	}
	st, body = status(t, c, http.MethodGet, "/_cat/snapshots", nil)
	if st != 400 || errType(body) != "action_request_validation_exception" {
		t.Fatalf("snapshots without repository: %d %v", st, body)
	}
}
