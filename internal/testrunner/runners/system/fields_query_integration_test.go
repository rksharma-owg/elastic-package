// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package system

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elastic/elastic-package/internal/elasticsearch"
	"github.com/elastic/elastic-package/internal/fields"
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

	sourceModes := []string{"stored"}
	if version.GreaterThanEqual(semver.MustParse("8.4.0")) {
		sourceModes = append(sourceModes, "synthetic")
	}
	for _, sourceMode := range sourceModes {
		for _, tc := range []fieldsQueryIntegrationCase{
			{name: "valid", documents: input.Documents[:1]},
			{name: "ignored", documents: input.Documents[1:], ignored: []string{"bad_date", "bad_keyword"}},
			{name: "mixed", documents: input.Documents, ignored: []string{"bad_date", "bad_keyword"}},
			{name: "user_field", documents: input.Documents, ignored: []string{"bad_date", "bad_keyword"}, userField: true},
			{name: "undefined_user_field", documents: input.Documents[1:], ignored: []string{"bad_date", "bad_keyword"}, undefinedField: true},
		} {
			t.Run(sourceMode+"/"+tc.name, func(t *testing.T) {
				ctx := t.Context()
				index, ignored := createFieldsQueryIndex(t, client, input.Mappings, tc, sourceMode == "synthetic")
				r := tester{esAPI: client.API}
				docs, err := r.getDocs(ctx, index)
				require.NoError(t, err)
				require.Len(t, docs.Source, len(tc.documents))
				assert.ElementsMatch(t, ignored, docs.IgnoredFields)
				if sourceMode == "synthetic" {
					validateFieldsQuerySyntheticDocs(t, docs, tc)
				}
				for _, fields := range docs.Fields {
					if tc.userField {
						assert.Equal(t, []any{"user value"}, fields["my_ignored"])
					} else {
						assert.NotContains(t, fields, "my_ignored")
					}
				}
				ds := scenarioDataStream{dataStream: index, ignoredFields: docs.IgnoredFields, degradedDocs: docs.DegradedDocs}
				err = validateIgnoredFields(version, ds, &testConfig{})
				if len(ignored) == 0 {
					require.NoError(t, err)
					assert.Empty(t, docs.DegradedDocs)
					return
				}
				var failure testrunner.ErrTestCaseFailed
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, "found ignored fields in data stream", failure.Reason)
				for _, field := range ignored {
					assert.Contains(t, failure.Details, field)
				}
				require.Len(t, docs.DegradedDocs, 2)
				require.NoError(t, validateIgnoredFields(version, ds, &testConfig{SkipIgnoredFields: ignored}))
			})
		}

	}
}

type fieldsQueryIntegrationCase struct {
	name           string
	documents      []json.RawMessage
	ignored        []string
	userField      bool
	undefinedField bool
}

func createFieldsQueryIndex(t *testing.T, client *elasticsearch.Client, inputMappings json.RawMessage, tc fieldsQueryIntegrationCase, synthetic bool) (string, []string) {
	t.Helper()
	ctx := t.Context()
	index := fmt.Sprintf("ep-ignored-test-%d", time.Now().UnixNano())
	var mappings map[string]any
	require.NoError(t, json.Unmarshal(inputMappings, &mappings))
	if synthetic {
		mappings["_source"] = map[string]string{"mode": "synthetic"}
		// Older synthetic source implementations cannot use ignore_malformed dates.
		// Exercise ignored keywords here; stored source cases cover malformed dates.
		delete(mappings["properties"].(map[string]any), "bad_date")
		if len(tc.ignored) > 0 {
			tc.ignored = []string{"bad_keyword"}
		}
	}
	if tc.userField {
		mappings["properties"].(map[string]any)["my_ignored"] = map[string]string{"type": "keyword"}
	}
	if tc.undefinedField {
		mappings["properties"].(map[string]any)["undefined_user_field"] = map[string]string{"type": "keyword"}
	}
	body, err := json.Marshal(map[string]any{"mappings": mappings})
	require.NoError(t, err)
	resp, err := client.Indices.Create(index, client.Indices.Create.WithContext(ctx), client.Indices.Create.WithBody(strings.NewReader(string(body))))
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
		if tc.userField || tc.undefinedField || synthetic {
			var value map[string]any
			require.NoError(t, json.Unmarshal(doc, &value))
			if synthetic {
				if value["bad_date"] != "2026-10-06" {
					value["bad_keyword"] = "oversize"
				}
				delete(value, "bad_date")
			}
			if tc.userField {
				value["my_ignored"] = "user value"
			}
			if tc.undefinedField {
				value["undefined_user_field"] = "not declared"
			}
			doc, err = json.Marshal(value)
			require.NoError(t, err)
		}
		resp, err := client.Index(index, strings.NewReader(string(doc)), client.Index.WithContext(ctx), client.Index.WithRefresh("true"))
		require.NoError(t, err)
		require.False(t, resp.IsError(), resp.String())
		require.NoError(t, resp.Body.Close())
	}
	return index, tc.ignored
}

func validateFieldsQuerySyntheticDocs(t *testing.T, docs *hits, tc fieldsQueryIntegrationCase) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "fields"), 0755))
	schema := "- name: '@timestamp'\n  type: date\n- name: bad_date\n  type: date\n- name: bad_keyword\n  type: keyword\n"
	if tc.userField {
		schema += "- name: my_ignored\n  type: keyword\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "fields", "fields.yml"), []byte(schema), 0644))
	repositoryRoot, err := os.OpenRoot(root)
	require.NoError(t, err)
	defer repositoryRoot.Close()
	validator, err := fields.CreateValidator(repositoryRoot, root, filepath.Join(root, "fields"), fields.WithDisabledDependencyManagement(), fields.WithDisableNormalization(true))
	require.NoError(t, err)
	validationErrors := validateFields(docs.getDocs(true), validator)
	if tc.undefinedField {
		require.NotEmpty(t, validationErrors)
		assert.Contains(t, validationErrors.Error(), `field "undefined_user_field" is undefined`)
	} else {
		require.Empty(t, validationErrors)
	}
}
