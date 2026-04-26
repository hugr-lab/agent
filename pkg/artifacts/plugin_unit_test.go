package artifacts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// formatUploadPlaceholder is the most important contract of the
// user-upload plugin: the LLM sees this exact text in place of the
// uploaded blob, so its shape decides whether the model can act on
// the upload (find the local_path, hand it to a tool) or just sees
// "here was a file, dunno what". Drives the unit test for the rich
// placeholder format.
func TestFormatUploadPlaceholder(t *testing.T) {
	cases := []struct {
		name      string
		ref       ArtifactRef
		mime      string
		localPath string
		wantHas   []string
		wantLacks []string
	}{
		{
			name: "full happy path with local_path",
			ref: ArtifactRef{
				ID: "art_ag01_42_aaaa", Name: "report.csv", Type: "csv",
				SizeBytes: 12345, Visibility: VisibilityUser,
			},
			mime:      "text/csv",
			localPath: "/var/data/artifacts/abc.csv",
			wantHas: []string{
				"[user-upload]",
				"artifact_id: art_ag01_42_aaaa",
				"name: report.csv",
				"type: csv",
				"mime: text/csv",
				"size_bytes: 12345",
				"visibility: user",
				"local_path: /var/data/artifacts/abc.csv",
				"pass it directly to python/duckdb/curl",
			},
		},
		{
			name: "no local path → no local_path line",
			ref: ArtifactRef{
				ID: "art_ag01_43_bbbb", Name: "remote.json", Type: "json",
				SizeBytes: 8, Visibility: VisibilitySelf,
			},
			mime:      "application/json",
			localPath: "",
			wantHas: []string{
				"artifact_id: art_ag01_43_bbbb",
				"mime: application/json",
				"visibility: self",
			},
			wantLacks: []string{
				"local_path:",
				"pass it directly",
			},
		},
		{
			name: "missing size → no size_bytes line",
			ref: ArtifactRef{
				ID: "art_ag01_44_cccc", Name: "x", Type: "bin",
				SizeBytes: 0, Visibility: VisibilityUser,
			},
			mime: "application/octet-stream",
			wantHas: []string{
				"artifact_id: art_ag01_44_cccc",
				"mime: application/octet-stream",
			},
			wantLacks: []string{
				"size_bytes:",
			},
		},
		{
			name: "header line is always first — survives prompt truncation",
			ref:  ArtifactRef{ID: "art_x", Name: "n", Type: "txt", Visibility: VisibilityUser},
			mime: "text/plain",
			wantHas: []string{
				"[user-upload]\n",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatUploadPlaceholder(tc.ref, tc.mime, tc.localPath)
			for _, want := range tc.wantHas {
				assert.True(t, strings.Contains(got, want),
					"placeholder should contain %q\n----\n%s", want, got)
			}
			for _, lack := range tc.wantLacks {
				assert.False(t, strings.Contains(got, lack),
					"placeholder should NOT contain %q\n----\n%s", lack, got)
			}
		})
	}
}

// TestUserUploadPlugin_NameOnly is the cheapest sanity check that
// Manager.UserUploadPlugin() returns a usable *plugin.Plugin (the
// constructor's args validate cleanly). Full callback exercise lives
// in the A2A integration test (#234) — that path needs a live ADK
// runner to construct an InvocationContext.
func TestUserUploadPlugin_Constructs(t *testing.T) {
	m := &Manager{cfg: Config{
		UploadDefaultVisibility: VisibilityUser,
		UploadDefaultTTL:        TTL7d,
	}, log: nopLogger()}
	p, err := m.UserUploadPlugin()
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "artifacts-user-upload", p.Name())
}
