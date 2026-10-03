// mcp_publish.go is the publish_artifact task tool (Story 78.18). Unlike
// every other tool it is not a CLI verb: it needs the session that
// `archivist connect` runs (its id and working directory), which only the
// daemon knows and passes as --publish-session and --publish-dir. The
// daemon passes them only when the session's task token was granted the
// publish scope, and keeps the tool out of every harness pre-allow list, so
// each call reaches the owner's approval card.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mosaicss/archivist/internal/artifact"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/cmd"
	"github.com/mosaicss/archivist/internal/taskscope"
)

// publishConfig is the session a task mode server may publish for.
type publishConfig struct {
	SessionID string
	Dir       string
}

// publishSessionRe is chat-api's agent id shape (a UUID).
var publishSessionRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// digestRe is a lowercase hex SHA-256.
var digestRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

// publishResult is the tool's structured result (and its JSON text).
type publishResult struct {
	ArtifactID string `json:"artifactId"`
	SessionID  string `json:"sessionId"`
	Name       string `json:"name"`
	MediaType  string `json:"mediaType"`
	Size       int64  `json:"size"`
	Digest     string `json:"digest"`
	CreatedAt  int64  `json:"createdAt"`
	URL        string `json:"url"`
}

const publishDescription = "Publish one file from your working directory to the user's Mosaic workspace, " +
	"where it opens beside the conversation. The path is relative to the working directory (or absolute " +
	"inside it); symbolic links are refused. Allowed: .pdf, .txt, .md, .csv and .json files up to 10 MiB " +
	"whose content matches the extension. Every call asks the user for approval. The result carries the " +
	"artifact id and its url."

// addPublishTool registers publish_artifact on a task mode server.
func addPublishTool(server *mcp.Server, version string, src tokenSource, pub publishConfig) {
	server.AddTool(&mcp.Tool{
		Name:        taskscope.PublishTool,
		Title:       "Publish artifact",
		Description: publishDescription,
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"path": {Type: "string", Description: "The file to publish, inside the working directory."},
			},
			Required:             []string{"path"},
			AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
		},
		Annotations: &mcp.ToolAnnotations{
			Title:           "Publish artifact",
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			OpenWorldHint:   boolPtr(true),
		},
	}, newPublishHandler(version, src, pub))
}

func newPublishHandler(version string, src tokenSource, pub publishConfig) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path, usageErr := publishPathArg(req.Params.Arguments)
		if usageErr != "" {
			return errorResult(cmd.ExitUsageError, usageErr, ""), nil
		}
		token, err := src()
		if err != nil {
			return errorResult(cmd.ExitAuthError, err.Error(), ""), nil
		}
		f, err := artifact.Load(pub.Dir, path)
		if err != nil {
			return errorResult(cmd.ExitUsageError, "publish_artifact refused: "+err.Error(), ""), nil
		}
		api := client.New(token, version)
		api.SetQuiet(true)
		var stderr bytes.Buffer
		api.SetStderr(&stderr)
		out, err := api.PublishArtifact(ctx, pub.SessionID, f.Name, f.MediaType, f.Data)
		if err != nil {
			return publishAPIError(err, stderr.String()), nil
		}
		if out.Digest != f.Digest || out.Size != int64(len(f.Data)) || out.SessionID != pub.SessionID ||
			out.MediaType != f.MediaType || !digestRe.MatchString(out.Digest) {
			return errorResult(cmd.ExitServerError, "chat-api stored an artifact that does not match the file sent", ""), nil
		}
		res := publishResult{ArtifactID: out.ArtifactID, SessionID: out.SessionID, Name: out.Name, MediaType: out.MediaType,
			Size: out.Size, Digest: out.Digest, CreatedAt: out.CreatedAt,
			URL: strings.TrimRight(api.BaseURL, "/") + "/artifacts/" + url.PathEscape(out.ArtifactID)}
		text, _ := json.Marshal(res)
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: string(text)}},
			StructuredContent: res,
		}, nil
	}
}

// publishPathArg decodes the arguments: exactly one string "path".
func publishPathArg(raw json.RawMessage) (string, string) {
	args := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Sprintf("invalid tool arguments JSON: %v", err)
		}
	}
	for k := range args {
		if k != "path" {
			return "", fmt.Sprintf("unknown argument %q for tool %s", k, taskscope.PublishTool)
		}
	}
	v, ok := args["path"]
	if !ok {
		return "", `missing required argument "path"`
	}
	s, ok := v.(string)
	if !ok {
		return "", `argument "path" must be a string`
	}
	return s, ""
}

// publishAPIError maps a failed upload onto the CLI exit codes.
func publishAPIError(err error, stderrText string) *mcp.CallToolResult {
	var apiErr *client.APIError
	var codeErr *client.ExitCodeError
	switch {
	case errors.As(err, &apiErr):
		code := cmd.ExitGenericError
		switch {
		case apiErr.Status == 401 || apiErr.Status == 403:
			code = cmd.ExitAuthError
		case apiErr.Status == 404:
			code = cmd.ExitNotFound
		case apiErr.Status >= 400 && apiErr.Status < 500:
			code = cmd.ExitUsageError
		}
		return errorResult(code, fmt.Sprintf("publish_artifact failed (HTTP %d): %s", apiErr.Status, apiErr.Error()), "")
	case errors.As(err, &codeErr):
		return errorResult(codeErr.Code, "publish_artifact failed: "+codeErr.Message+"\n"+stderrText, "")
	default:
		return errorResult(cmd.ExitGenericError, "publish_artifact failed: "+err.Error(), "")
	}
}
