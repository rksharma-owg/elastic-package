import json
import pathlib
import sys
import urllib.error
import urllib.request


def request(port, path, data=None, method="GET"):
    with urllib.request.urlopen(urllib.request.Request(
        f"http://127.0.0.1:{port}/{path}",
        data=json.dumps(data).encode() if data is not None else None,
        headers={"Content-Type": "application/json"}, method=method,
    ), timeout=120) as response:
        return json.load(response)


code = pathlib.Path(sys.argv[1])
query = json.loads((code / "internal/testrunner/runners/system/tester.go").read_text().split("const FieldsQuery = `")[1].split("`")[0])
for port in (19212, 19234):
    print(json.dumps(request(port, "")))
request(19212, "legacy", {"mappings": {"properties": {"legacy_bad": {"type": "date", "ignore_malformed": True}, "my_ignored": {"type": "keyword"}}}}, "PUT")
request(19212, "legacy/_doc/1?refresh=true", {"legacy_bad": "invalid", "my_ignored": "user value"}, "PUT")
request(19212, "_snapshot/compatibility", {"type": "fs", "settings": {"location": "/snapshots"}}, "PUT")
snapshot = request(19212, "_snapshot/compatibility/old?wait_for_completion=true", {"indices": "legacy", "include_global_state": False}, "PUT")
assert snapshot["snapshot"]["state"] == "SUCCESS", snapshot
request(19234, "_snapshot/compatibility", {"type": "fs", "settings": {"location": "/snapshots", "readonly": True}}, "PUT")
request(19234, "_snapshot/compatibility/old/_restore?wait_for_completion=true", {"indices": "legacy", "include_global_state": False}, "POST")
request(19234, "_cluster/health/legacy?wait_for_status=yellow&timeout=90s")
request(19234, "modern", {"mappings": {"properties": {"modern_bad": {"type": "date", "ignore_malformed": True}, "my_ignored": {"type": "keyword"}}}}, "PUT")
request(19234, "modern/_doc/1?refresh=true", {"modern_bad": "invalid", "my_ignored": "user value"}, "PUT")
settings = request(19234, "legacy,modern/_settings")
print("INDEX CREATION VERSIONS", json.dumps(settings))
result = request(19234, "legacy,modern/_search", query, "POST")
print("MIXED INDEX QUERY", json.dumps(result))
assert result["_shards"]["failed"] == 0, result
assert result["aggregations"]["all_ignored"]["doc_count"] == 2, result
assert {bucket["key"] for bucket in result["aggregations"]["all_ignored"]["ignored_fields"]["buckets"]} == {"legacy_bad", "modern_bad"}, result
assert len(result["aggregations"]["all_ignored"]["ignored_docs"]["hits"]["hits"]) == 2, result
assert all(hit["fields"]["my_ignored"] == ["user value"] for hit in result["hits"]["hits"]), result
query["aggs"]["all_ignored"]["aggs"]["ignored_fields"]["terms"]["script"]["source"] = query["aggs"]["all_ignored"]["aggs"]["ignored_fields"]["terms"]["script"]["source"].replace("ignored = doc['_ignored'];", "throw new IllegalArgumentException('unexpected lookup error');")
try:
    request(19234, "legacy,modern/_search", query, "POST")
except urllib.error.HTTPError as error:
    detail = error.read().decode()
    assert error.code == 400 and "unexpected lookup error" in detail, detail
    print("UNEXPECTED ERROR PROPAGATES", detail)
else:
    raise AssertionError("Unexpected errors must not be swallowed")
