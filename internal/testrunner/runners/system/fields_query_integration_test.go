// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package system

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elastic/elastic-package/internal/elasticsearch"
	"github.com/elastic/elastic-package/internal/testrunner"
)

// Run against an isolated Elasticsearch node to exercise Painless and both
// representations of _ignored, rather than mocking the query response.
func TestFieldsQueryIntegration(t *testing.T) {
	host := os.Getenv("ELASTIC_PACKAGE_IGNORED_FIELDS_TEST_HOST")
	if host == "" {
		t.Skip("set ELASTIC_PACKAGE_IGNORED_FIELDS_TEST_HOST to an isolated Elasticsearch node")
	}
	client, err := elasticsearch.NewClient(elasticsearch.OptionWithAddress(host))
	require.NoError(t, err)
	ctx := t.Context()
	info, err := client.Info(ctx)
	require.NoError(t, err)
	version, err := semver.NewVersion(info.Version.Number)
	require.NoError(t, err)
	t.Logf("Elasticsearch %s", version)

	fixture, err := os.ReadFile("testdata/ignored-fields-index.json")
	require.NoError(t, err)
	var input struct {
		Mappings  json.RawMessage   `json:"mappings"`
		Documents []json.RawMessage `json:"documents"`
	}
	require.NoError(t, json.Unmarshal(fixture, &input))
	require.Len(t, input.Documents, 3)

	for _, tc := range []struct {
		name      string
		documents []json.RawMessage
		ignored   []string
	}{
		{name: "valid", documents: input.Documents[:1]},
		{name: "ignored", documents: input.Documents[1:], ignored: []string{"bad_date", "bad_keyword"}},
		{name: "mixed", documents: input.Documents, ignored: []string{"bad_date", "bad_keyword"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			index := fmt.Sprintf("ep-ignored-test-%d", time.Now().UnixNano())
			body := fmt.Sprintf(`{"mappings":%s}`, input.Mappings)
			resp, err := client.Indices.Create(index, client.Indices.Create.WithContext(ctx), client.Indices.Create.WithBody(strings.NewReader(body)))
			require.NoError(t, err)
			defer resp.Body.Close()
			require.False(t, resp.IsError(), resp.String())
			t.Cleanup(func() {
				resp, err := client.Indices.Delete([]string{index})
				require.NoError(t, err)
				defer resp.Body.Close()
				require.False(t, resp.IsError(), resp.String())
			})
			for _, doc := range tc.documents {
				resp, err := client.Index(index, strings.NewReader(string(doc)), client.Index.WithContext(ctx), client.Index.WithRefresh("true"))
				require.NoError(t, err)
				require.False(t, resp.IsError(), resp.String())
				require.NoError(t, resp.Body.Close())
			}
			r := tester{esAPI: client.API}
			docs, err := r.getDocs(ctx, index)
			require.NoError(t, err)
			require.Len(t, docs.Source, len(tc.documents))
			assert.ElementsMatch(t, tc.ignored, docs.IgnoredFields)
			ds := scenarioDataStream{dataStream: index, ignoredFields: docs.IgnoredFields, degradedDocs: docs.DegradedDocs}
			err = validateIgnoredFields(version, ds, &testConfig{})
			if len(tc.ignored) == 0 {
				require.NoError(t, err)
				assert.Empty(t, docs.DegradedDocs)
				return
			}
			var failure testrunner.ErrTestCaseFailed
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, "found ignored fields in data stream", failure.Reason)
			for _, field := range tc.ignored {
				assert.Contains(t, failure.Details, field)
			}
			require.Len(t, docs.DegradedDocs, 2)
			require.NoError(t, validateIgnoredFields(version, ds, &testConfig{SkipIgnoredFields: tc.ignored}))
		})
	}
}
