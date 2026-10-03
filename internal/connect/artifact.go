package connect

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/mosaicss/archivist/internal/artifact"
	"github.com/mosaicss/archivist/internal/taskscope"
)

// Story 78.18: a successful publish_artifact call becomes one data-artifact
// chunk right after its tool output. Both translators already report the
// call (tool-input-*) and its outcome (tool-output-*); the tracker follows
// the publish calls by toolCallId and reads the artifact from the output.
// A failed or denied call emits nothing.

// publishToolNames are the names each harness reports for publish_artifact.
var publishToolNames = map[string]bool{
	"mcp__archivist__" + taskscope.PublishTool: true, // Claude Code
	"archivist." + taskscope.PublishTool:       true, // Codex (server.tool)
}

var artifactDigestRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

// artifactTracker turns publish_artifact outputs into data-artifact chunks.
type artifactTracker struct {
	// base is the chat-api base URL the artifact read route hangs off.
	base  string
	calls map[string]bool
}

func newArtifactTracker(base string) *artifactTracker {
	return &artifactTracker{base: strings.TrimRight(base, "/"), calls: map[string]bool{}}
}

// observe passes chunks through, appending a data-artifact after each
// successful publish_artifact output.
func (a *artifactTracker) observe(chunks []Chunk) []Chunk {
	if len(chunks) == 0 {
		return chunks
	}
	out := make([]Chunk, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, c)
		id, _ := c["toolCallId"].(string)
		if id == "" {
			continue
		}
		switch c["type"] {
		case "tool-input-start", "tool-input-available":
			if name, _ := c["toolName"].(string); publishToolNames[name] {
				a.calls[id] = true
			}
		case "tool-output-available":
			if a.calls[id] {
				delete(a.calls, id)
				if art := a.chunk(c["output"]); art != nil {
					out = append(out, art)
				}
			}
		case "tool-output-error", "tool-output-denied":
			delete(a.calls, id)
		}
	}
	return out
}

// reset forgets open calls (a new harness process).
func (a *artifactTracker) reset() { a.calls = map[string]bool{} }

// publishedArtifact is the part of publish_artifact's result the chunk needs.
type publishedArtifact struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name"`
	MediaType  string `json:"mediaType"`
	Digest     string `json:"digest"`
}

// chunk builds the data-artifact chunk from a tool output: Codex passes the
// MCP result ({content, structuredContent}), Claude Code its content blocks
// (or a string). nil when no valid artifact is found.
func (a *artifactTracker) chunk(output any) Chunk {
	for _, cand := range artifactCandidates(output) {
		var p publishedArtifact
		if json.Unmarshal(cand, &p) != nil || !validArtifact(p) {
			continue
		}
		return Chunk{"type": "data-artifact", "data": map[string]any{
			"artifactId": p.ArtifactID, "name": p.Name, "mediaType": p.MediaType, "digest": p.Digest,
			"url": a.base + "/artifacts/" + url.PathEscape(p.ArtifactID),
		}}
	}
	return nil
}

func validArtifact(p publishedArtifact) bool {
	if p.ArtifactID == "" || strings.TrimSpace(p.Name) == "" || !artifactDigestRe.MatchString(p.Digest) {
		return false
	}
	for _, mt := range artifact.MediaTypes {
		if p.MediaType == mt {
			return true
		}
	}
	return false
}

// artifactCandidates lists the JSON documents an output may carry the
// artifact in, most structured first.
func artifactCandidates(output any) [][]byte {
	var out [][]byte
	var texts []string
	addBlocks := func(blocks []any) {
		for _, b := range blocks {
			m, _ := b.(map[string]any)
			if m["type"] == "text" {
				if t, ok := m["text"].(string); ok {
					texts = append(texts, t)
				}
			}
		}
	}
	switch v := output.(type) {
	case map[string]any:
		if sc, ok := v["structuredContent"].(map[string]any); ok {
			if b, err := json.Marshal(sc); err == nil {
				out = append(out, b)
			}
		}
		if blocks, ok := v["content"].([]any); ok {
			addBlocks(blocks)
		}
	case []any:
		addBlocks(v)
	case string:
		texts = append(texts, v)
	}
	for _, t := range texts {
		out = append(out, []byte(t))
	}
	if len(texts) > 1 {
		out = append(out, []byte(strings.Join(texts, "")))
	}
	return out
}
