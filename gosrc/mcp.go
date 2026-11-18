// MCP endpoint (POST /mcp): JSON-RPC 2.0 Streamable HTTP, modern era
// 2026-07-28 (server/discover) with a legacy initialize fallback. Plain
// JSON responses; no sessions.
package main

import (
	"encoding/json"
	"strconv"
)

const mcpVersion = "2026-07-28"

// ---- tools ---------------------------------------------------------------

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func mcpTools() []mcpToolDef {
	strProp := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	return []mcpToolDef{
		{
			Name:        "create_repo",
			Description: "Create a repository for the viewer.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":        strProp("Repository name"),
					"description": strProp("Repository description"),
					"private":     map[string]any{"type": "boolean", "description": "Whether the repository is private"},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "get_repo",
			Description: "Get a repository by owner and name.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": strProp("Repository owner"),
					"repo":  strProp("Repository name"),
				},
				"required": []string{"owner", "repo"},
			},
		},
		{
			Name:        "create_issue",
			Description: "Create an issue in a repository.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": strProp("Repository owner"),
					"repo":  strProp("Repository name"),
					"title": strProp("Issue title"),
					"body":  strProp("Issue body"),
				},
				"required": []string{"owner", "repo", "title"},
			},
		},
		{
			Name:        "list_issues",
			Description: "List issues in a repository.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": strProp("Repository owner"),
					"repo":  strProp("Repository name"),
					"state": strProp("Issue state: open, closed, or all"),
				},
				"required": []string{"owner", "repo"},
			},
		},
		{
			Name:        "create_pull_request",
			Description: "Create a pull request in a repository.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": strProp("Repository owner"),
					"repo":  strProp("Repository name"),
					"title": strProp("Pull request title"),
					"head":  strProp("Head branch name"),
					"base":  strProp("Base branch name"),
					"body":  strProp("Pull request body"),
				},
				"required": []string{"owner", "repo", "title", "head", "base"},
			},
		},
		{
			Name:        "merge_pull_request",
			Description: "Merge a pull request with the squash method.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner":       strProp("Repository owner"),
					"repo":        strProp("Repository name"),
					"pull_number": map[string]any{"type": "integer", "description": "Pull request number"},
				},
				"required": []string{"owner", "repo", "pull_number"},
			},
		},
	}
}

// mcpAliases maps alternative tool names to canonical ones.
var mcpAliases = map[string]string{
	"create_repository": "create_repo", "repo_create": "create_repo", "repos_create": "create_repo", "repository_create": "create_repo",
	"get_repository": "get_repo", "repo_get": "get_repo", "repos_get": "get_repo", "repository_get": "get_repo",
	"issue_create": "create_issue", "issues_create": "create_issue",
	"issues_list": "list_issues", "issue_list": "list_issues",
	"pull_request_create": "create_pull_request", "pulls_create": "create_pull_request", "create_pr": "create_pull_request", "pr_create": "create_pull_request",
	"pull_request_merge": "merge_pull_request", "pulls_merge": "merge_pull_request", "merge_pr": "merge_pull_request", "pr_merge": "merge_pull_request",
}

// ---- JSON-RPC plumbing ----------------------------------------------------

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func mcpResult(id json.RawMessage, result any) []byte {
	out := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(nullIfEmpty(id)), "result": result}
	b, _ := json.Marshal(out)
	return jsonResp(200, b)
}

func mcpError(id json.RawMessage, code int, message string) []byte {
	out := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(nullIfEmpty(id)), "error": map[string]any{"code": code, "message": message}}
	b, _ := json.Marshal(out)
	return jsonResp(200, b)
}

func nullIfEmpty(id json.RawMessage) []byte {
	if len(id) == 0 {
		return []byte("null")
	}
	return id
}

// toolResult renders a tools/call success (or a tool-level error).
func mcpToolResult(id json.RawMessage, obj any, isErr bool, errText string) []byte {
	var text string
	if errText != "" {
		text = errText
	} else {
		b, _ := json.Marshal(obj)
		text = string(b)
	}
	result := map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": obj,
	}
	if isErr {
		result["isError"] = true
	}
	return mcpResult(id, result)
}

// ---- handler ---------------------------------------------------------------

// routeMCP handles POST /mcp.
func (s *server) routeMCP(req *request) []byte {
	if !s.authorized(req) {
		return errResp(401, "Requires authentication")
	}
	if req.method != "POST" {
		return errResp(405, "Method Not Allowed")
	}
	var rpc mcpRequest
	if err := json.Unmarshal(req.body, &rpc); err != nil {
		// Non-JSON body: HTTP 400 with a -32700 parse error, id null.
		out := map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error"}}
		b, _ := json.Marshal(out)
		return newResp(400, "application/json", b)
	}
	if rpc.Method == "" {
		return mcpError(rpc.ID, -32600, "Invalid Request")
	}
	// Notifications carry no id: acknowledge with 202 and no body.
	if len(rpc.ID) == 0 {
		return newResp(202, "application/json", []byte{})
	}
	switch rpc.Method {
	case "server/discover":
		serverInfo := map[string]any{"name": "go-forge", "version": "1.0.0"}
		return mcpResult(rpc.ID, map[string]any{
			"protocolVersion":   mcpVersion,
			"supportedVersions": []string{mcpVersion, "2025-11-25"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        serverInfo,
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": serverInfo},
		})
	case "initialize":
		// Legacy handshake: echo the client's offered version when it is
		// one we speak, else the preferred revision.
		version := mcpVersion
		var params map[string]any
		if len(rpc.Params) > 0 {
			_ = json.Unmarshal(rpc.Params, &params)
			if v, ok := params["protocolVersion"].(string); ok && v != "" {
				version = v
			}
		}
		return mcpResult(rpc.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "go-forge", "version": "1.0.0"},
		})
	case "tools/list":
		return mcpResult(rpc.ID, map[string]any{"tools": mcpTools()})
	case "tools/call":
		return s.mcpCallTool(rpc)
	case "ping":
		return mcpResult(rpc.ID, map[string]any{})
	}
	return mcpError(rpc.ID, -32601, "Method not found: "+rpc.Method)
}

func (s *server) mcpCallTool(rpc mcpRequest) []byte {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(rpc.Params) > 0 {
		if err := json.Unmarshal(rpc.Params, &params); err != nil {
			return mcpError(rpc.ID, -32602, "Invalid params")
		}
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}
	name := params.Name
	if canon, ok := mcpAliases[name]; ok {
		name = canon
	}
	args := params.Arguments
	argStr := func(k string) string {
		if v, ok := args[k].(string); ok {
			return v
		}
		return ""
	}
	argInt := func(k string) (int, bool) {
		v, ok := args[k]
		if !ok {
			return 0, false
		}
		switch t := v.(type) {
		case float64:
			return int(t), true
		case int64:
			return int(t), true
		case int:
			return t, true
		case string:
			if n, err := strconv.Atoi(t); err == nil {
				return n, true
			}
		}
		return 0, false
	}
	// Required-argument check: missing required args are a JSON-RPC
	// -32602 (the client also accepts isError:true, but -32602 is the
	// canonical answer).
	required := map[string][]string{
		"create_repo":          {"name"},
		"get_repo":             {"owner", "repo"},
		"create_issue":         {"owner", "repo", "title"},
		"list_issues":          {"owner", "repo"},
		"create_pull_request":  {"owner", "repo", "title", "head", "base"},
		"merge_pull_request":   {"owner", "repo", "pull_number"},
	}
	if reqs, ok := required[name]; ok {
		for _, k := range reqs {
			if _, present := args[k]; !present {
				return mcpError(rpc.ID, -32602, "Missing required argument: "+k)
			}
			if k != "pull_number" && argStr(k) == "" {
				return mcpError(rpc.ID, -32602, "Missing required argument: "+k)
			}
		}
	} else {
		return mcpError(rpc.ID, -32602, "Unknown tool: "+params.Name)
	}

	switch name {
	case "create_repo":
		s.store.mu.Lock()
		if s.store.getRepo(viewerLogin, argStr("name")) != nil {
			s.store.mu.Unlock()
			return mcpToolResult(rpc.ID, nil, true, "Repository already exists")
		}
		r := s.store.newRepo(viewerLogin, argStr("name"), argStr("description"), false)
		if v, ok := args["private"].(bool); ok {
			r.Private = v
		}
		s.store.mu.Unlock()
		return mcpToolResult(rpc.ID, repoObject(r), false, "")
	case "get_repo":
		s.store.mu.RLock()
		r := s.store.getRepo(argStr("owner"), argStr("repo"))
		s.store.mu.RUnlock()
		if r == nil {
			return mcpToolResult(rpc.ID, nil, true, "repository not found: "+argStr("owner")+"/"+argStr("repo"))
		}
		return mcpToolResult(rpc.ID, repoObject(r), false, "")
	case "create_issue":
		s.store.mu.Lock()
		r := s.store.getRepo(argStr("owner"), argStr("repo"))
		if r == nil {
			s.store.mu.Unlock()
			return mcpToolResult(rpc.ID, nil, true, "repository not found: "+argStr("owner")+"/"+argStr("repo"))
		}
		iss := r.newIssue(argStr("title"), argStr("body"), nil)
		obj := issueObject(r, iss)
		s.store.mu.Unlock()
		return mcpToolResult(rpc.ID, obj, false, "")
	case "list_issues":
		state := argStr("state")
		if state == "" {
			state = "open"
		}
		s.store.mu.RLock()
		r := s.store.getRepo(argStr("owner"), argStr("repo"))
		if r == nil {
			s.store.mu.RUnlock()
			return mcpToolResult(rpc.ID, nil, true, "repository not found: "+argStr("owner")+"/"+argStr("repo"))
		}
		var list []any
		for _, n := range r.IssueOrder {
			iss := r.Issues[n]
			if state != "all" && iss.State != state {
				continue
			}
			list = append(list, issueObject(r, iss))
		}
		if list == nil {
			list = []any{}
		}
		s.store.mu.RUnlock()
		return mcpToolResult(rpc.ID, map[string]any{"issues": list}, false, "")
	case "create_pull_request":
		s.store.mu.Lock()
		r := s.store.getRepo(argStr("owner"), argStr("repo"))
		if r == nil {
			s.store.mu.Unlock()
			return mcpToolResult(rpc.ID, nil, true, "repository not found: "+argStr("owner")+"/"+argStr("repo"))
		}
		p, errMsg := r.newPull(argStr("title"), argStr("body"), argStr("head"), argStr("base"))
		if errMsg != "" {
			s.store.mu.Unlock()
			return mcpToolResult(rpc.ID, nil, true, errMsg)
		}
		obj := pullObject(r, p)
		s.store.mu.Unlock()
		return mcpToolResult(rpc.ID, obj, false, "")
	case "merge_pull_request":
		num, _ := argInt("pull_number")
		s.store.mu.Lock()
		r := s.store.getRepo(argStr("owner"), argStr("repo"))
		if r == nil {
			s.store.mu.Unlock()
			return mcpToolResult(rpc.ID, nil, true, "repository not found: "+argStr("owner")+"/"+argStr("repo"))
		}
		p := r.Pulls[num]
		if p == nil {
			s.store.mu.Unlock()
			return mcpToolResult(rpc.ID, nil, true, "pull request not found")
		}
		mergeSHA, reason := r.doMerge(p)
		if reason == "conflict" {
			s.store.mu.Unlock()
			return mcpToolResult(rpc.ID, nil, true, "Merge conflict")
		}
		obj := pullObject(r, p)
		obj["merge_sha"] = mergeSHA
		s.store.mu.Unlock()
		return mcpToolResult(rpc.ID, obj, false, "")
	}
	return mcpError(rpc.ID, -32602, "Unknown tool: "+params.Name)
}

// repoObject renders a repo as a plain JSON object for MCP results.
func repoObject(r *Repo) map[string]any {
	return map[string]any{
		"id":           r.ID,
		"name":         r.Name,
		"full_name":    r.Owner + "/" + r.Name,
		"owner":        map[string]any{"login": r.Owner},
		"description":  r.Description,
		"private":      r.Private,
		"default_branch": r.DefaultBranch,
		"html_url":     "https://github.com/" + r.Owner + "/" + r.Name,
	}
}

func issueObject(r *Repo, iss *Issue) map[string]any {
	var labels []any
	for _, l := range iss.Labels {
		labels = append(labels, map[string]any{"name": l})
	}
	if labels == nil {
		labels = []any{}
	}
	return map[string]any{
		"id":     r.ID*1000000 + iss.Number,
		"number": iss.Number,
		"title":  iss.Title,
		"body":   iss.Body,
		"state":  iss.State,
		"labels": labels,
	}
}

func pullObject(r *Repo, p *Pull) map[string]any {
	return map[string]any{
		"id":           r.ID*1000000 + p.Number,
		"number":       p.Number,
		"title":        p.Title,
		"body":         p.Body,
		"state":        p.State,
		"merged":       p.Merged,
		"head":         map[string]any{"ref": p.Head, "sha": p.HeadSHA},
		"base":         map[string]any{"ref": p.Base, "sha": p.BaseSHA},
		"headRefName":  p.Head,
		"baseRefName":  p.Base,
		"mergeable":    !p.Merged && p.State == "open" && !r.prConflict(p),
		"merge_sha":    p.MergeSHA,
		"merged_at":    p.MergedAt,
	}
}
