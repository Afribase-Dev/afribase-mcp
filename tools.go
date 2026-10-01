package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Tools
//
// Every tool here is a thin translation: take the arguments, call one or more
// endpoints of Afribase's public REST API with this process's own access
// token, and shape the reply. Nothing is returned that was not deliberately
// projected - a Project carries its database password and JWT secret in the
// API's own response to its owner, and this bridge strips those before a
// tool result ever reaches a model.
// ─────────────────────────────────────────────────────────────────────────────

type mcpTool struct {
	Name        string
	Title       string
	Description string
	InputSchema map[string]any
	Group       string
	ReadOnly    bool
	Destructive bool
	// UntrustedData marks a tool whose result carries text somebody else
	// wrote: rows from a customer's database, a commit message, a line of
	// application output.
	UntrustedData bool
	Handler       func(*Bridge, *http.Request, json.RawMessage) (any, error)
}

var mcpTools = map[string]mcpTool{}

func register(t mcpTool) {
	if t.Group == "" {
		panic("MCP tool " + t.Name + " belongs to no feature group")
	}
	mcpTools[t.Name] = t
}

func mcpToolDescriptors(scope mcpScope) []map[string]any {
	names := make([]string, 0, len(mcpTools))
	for name := range mcpTools {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		t := mcpTools[name]
		if !scope.allows(t) {
			continue
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"title":       t.Title,
			"description": t.Description,
			"inputSchema": t.InputSchema,
			"annotations": map[string]any{
				"title":           t.Title,
				"readOnlyHint":    t.ReadOnly,
				"destructiveHint": t.Destructive,
			},
		})
	}
	return out
}

func object(required []string, props map[string]any) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func integer(description string, def int) map[string]any {
	return map[string]any{"type": "integer", "description": description, "default": def}
}

func mcpArgs(raw json.RawMessage, into any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return toolFailure("the arguments were not valid: %v", err)
	}
	return nil
}

// project is everything about a project that is safe to send onwards. The
// API's own response to its owner carries the database password, service
// key and JWT secret - this keeps only what a tool result should.
type project struct {
	ID          string `json:"id"`
	Ref         string `json:"ref"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Region      string `json:"region"`
	Plan        string `json:"plan"`
	OrgID       string `json:"organizationId"`
	CreatedAt   string `json:"createdAt"`
}

func projectSummary(p *project) map[string]any {
	return map[string]any{
		"id": p.ID, "ref": p.Ref, "name": p.Name, "slug": p.Slug,
		"description": p.Description, "kind": p.Kind, "status": p.Status,
		"region": p.Region, "plan": p.Plan, "createdAt": p.CreatedAt,
	}
}

// resolveProject resolves an identifier - slug or id - to a project the
// caller may see, honouring a connection pinned to one project.
func (b *Bridge) resolveProject(r *http.Request, identifier string) (*project, error) {
	if pinned := mcpScopeFrom(r).Project; pinned != "" {
		identifier = pinned
	}
	if strings.TrimSpace(identifier) == "" {
		return nil, toolFailure("name a project by its slug or id")
	}
	var p project
	if err := b.api.get(r.Context(), "/api/projects/"+queryEscape(identifier), &p); err != nil {
		return nil, toolFailure("no project called %q; call list_projects to see the ones you can reach", identifier)
	}
	return &p, nil
}

func init() {
	// ── Control plane ────────────────────────────────────────────────────

	register(mcpTool{
		Name: "list_projects", Group: "account", Title: "List projects", ReadOnly: true,
		Description: "List the Afribase projects this account can reach, with " +
			"each project's slug, kind, status and region. Start here when a " +
			"request names a project you have not seen before.",
		InputSchema: object(nil, map[string]any{"limit": integer("How many projects to return. Defaults to 50.", 50)}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Limit int }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			if a.Limit <= 0 || a.Limit > 200 {
				a.Limit = 50
			}
			var projects []project
			if err := b.api.get(r.Context(), "/api/projects", &projects); err != nil {
				return nil, err
			}
			if len(projects) > a.Limit {
				projects = projects[:a.Limit]
			}
			out := make([]map[string]any, 0, len(projects))
			for i := range projects {
				out = append(out, projectSummary(&projects[i]))
			}
			return map[string]any{"projects": out, "total": len(out)}, nil
		},
	})

	register(mcpTool{
		Name: "get_project", Group: "account", UntrustedData: true, Title: "Get a project", ReadOnly: true,
		Description: "Details of one project: its status, kind, plan and " +
			"region. Credentials are never returned; read those from the " +
			"project's database settings page.",
		InputSchema: object([]string{"project"}, map[string]any{"project": str("The project's slug or id.")}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			return projectSummary(p), nil
		},
	})

	register(mcpTool{
		Name: "get_project_usage", Group: "account", Title: "Get usage against plan limits", ReadOnly: true,
		Description: "How much of a project's database, storage, bandwidth " +
			"and auth-user allowance is in use, each alongside the plan's limit.",
		InputSchema: object([]string{"project"}, map[string]any{"project": str("The project's slug or id.")}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			var usage any
			if err := b.api.get(r.Context(), "/api/projects/"+p.ID+"/usage", &usage); err != nil {
				return nil, err
			}
			return usage, nil
		},
	})

	register(mcpTool{
		Name: "create_project", Group: "account", Title: "Create a project",
		Description: "Create a new Afribase project: a Postgres database " +
			"with auth, storage and a REST API in front of it. The database " +
			"password is generated and deliberately not returned - read it " +
			"from the project's database settings page.",
		InputSchema: object([]string{"name"}, map[string]any{
			"name":        str("A name for the project, at least 3 characters."),
			"description": str("What the project is for. Optional."),
			"kind": map[string]any{"type": "string", "enum": []string{"backend", "hosting"},
				"description": "backend for a database-first project, hosting for deploying from a git repository. Defaults to backend."},
			"organizationId": str("The organization to create it in. Optional; defaults to the account's first organization."),
		}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct {
				Name, Description, Kind, OrganizationID string
			}
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			if len(strings.TrimSpace(a.Name)) < 3 {
				return nil, toolFailure("a project name needs at least 3 characters")
			}
			orgID := a.OrganizationID
			if orgID == "" {
				var orgs []struct {
					ID string `json:"id"`
				}
				if err := b.api.get(r.Context(), "/api/organizations", &orgs); err != nil || len(orgs) == 0 {
					return nil, toolFailure("no organization to create the project in; pass organizationId")
				}
				orgID = orgs[0].ID
			}
			password, err := generatedPassword()
			if err != nil {
				return nil, err
			}
			var p project
			body := map[string]any{
				"name": a.Name, "description": a.Description, "kind": a.Kind,
				"databasePassword": password,
			}
			if err := b.api.post(r.Context(), "/api/organizations/"+orgID+"/projects", body, &p); err != nil {
				return nil, toolFailure("could not create the project: %v", err)
			}
			summary := projectSummary(&p)
			summary["note"] = "Provisioning runs in the background. The database password was generated and is on the project's database settings page."
			return summary, nil
		},
	})

	register(mcpTool{
		Name: "get_current_user", Group: "account", ReadOnly: true, UntrustedData: true, Title: "Who am I connected as",
		Description: "The Afribase account this bridge acts as, and the " +
			"organizations it belongs to. Call this when the projects you " +
			"expect are missing.",
		InputSchema: object(nil, nil),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var user struct {
				Email     string `json:"email"`
				FullName  string `json:"fullName"`
				CreatedAt string `json:"createdAt"`
			}
			if err := b.api.get(r.Context(), "/api/user", &user); err != nil {
				return nil, toolFailure("this token is not linked to an Afribase account")
			}
			out := map[string]any{"email": user.Email, "fullName": user.FullName, "joined": user.CreatedAt}
			var orgs []struct{ ID, Name, Slug, Plan string }
			if err := b.api.get(r.Context(), "/api/organizations", &orgs); err == nil {
				names := make([]map[string]any, 0, len(orgs))
				for _, o := range orgs {
					names = append(names, map[string]any{"id": o.ID, "name": o.Name, "slug": o.Slug, "plan": o.Plan})
				}
				out["organizations"] = names
			}
			return out, nil
		},
	})

	// ── Data ─────────────────────────────────────────────────────────────

	register(mcpTool{
		Name: "list_tables", Group: "database", UntrustedData: true, Title: "List database tables", ReadOnly: true,
		Description: "The tables in a project's database, with row counts " +
			"and sizes. Use this before writing any query, so the names are real.",
		InputSchema: object([]string{"project"}, map[string]any{"project": str("The project's slug or id.")}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			var tables any
			if err := b.api.get(r.Context(), "/api/projects/"+p.ID+"/tables", &tables); err != nil {
				return nil, err
			}
			return map[string]any{"tables": tables}, nil
		},
	})

	register(mcpTool{
		Name: "run_sql", Group: "database", UntrustedData: true, Title: "Run a read-only query", ReadOnly: true,
		Description: "Run one read-only SQL query against a project's " +
			"database and return the rows. Only SELECT and WITH are " +
			"accepted, one statement at a time, and the result is capped. " +
			"To change data, use apply_migration.",
		InputSchema: object([]string{"project", "query"}, map[string]any{
			"project": str("The project's slug or id."),
			"query":   str("A single SELECT or WITH statement."),
			"limit":   integer("Maximum rows to return, up to 500. Defaults to 100.", 100),
		}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct {
				Project, Query string
				Limit          int
			}
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			return b.runReadOnlyQuery(r, p.ID, a.Query, a.Limit)
		},
	})

	// ── Hosting ──────────────────────────────────────────────────────────

	register(mcpTool{
		Name: "list_apps", Group: "hosting", UntrustedData: true, Title: "List deployed apps", ReadOnly: true,
		Description: "The applications deployed in a project, with each app's status, repository, branch and public URL.",
		InputSchema: object([]string{"project"}, map[string]any{"project": str("The project's slug or id.")}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			var apps any
			if err := b.api.get(r.Context(), "/api/projects/"+p.ID+"/apps", &apps); err != nil {
				return nil, err
			}
			return map[string]any{"apps": apps}, nil
		},
	})

	register(mcpTool{
		Name: "get_app_logs", Group: "hosting", UntrustedData: true, Title: "Read an app's logs", ReadOnly: true,
		Description: "The most recent lines an app has written to stdout and stderr.",
		InputSchema: object([]string{"project", "app"}, map[string]any{
			"project": str("The project's slug or id."),
			"app":     str("The app's slug or id."),
			"tail":    integer("How many lines from the end. Defaults to 100, up to 1000.", 100),
		}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct {
				Project, App string
				Tail         int
			}
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			if a.Tail <= 0 || a.Tail > 1000 {
				a.Tail = 100
			}
			var lines any
			path := fmt.Sprintf("/api/projects/%s/apps/%s/logs?tail=%d", p.ID, queryEscape(a.App), a.Tail)
			if err := b.api.get(r.Context(), path, &lines); err != nil {
				return nil, toolFailure("could not read the logs: %v", err)
			}
			return map[string]any{"lines": lines}, nil
		},
	})

	register(mcpTool{
		Name: "list_deploys", Group: "hosting", UntrustedData: true, Title: "List an app's deploys", ReadOnly: true,
		Description: "The deploy history for one app: what was built, from which commit, what triggered it and whether it succeeded.",
		InputSchema: object([]string{"project", "app"}, map[string]any{
			"project": str("The project's slug or id."),
			"app":     str("The app's slug or id."),
			"limit":   integer("How many deploys, most recent first. Defaults to 10.", 10),
		}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			deploys, err := b.listDeploys(r, raw)
			if err != nil {
				return nil, err
			}
			return map[string]any{"deploys": deploys}, nil
		},
	})

	register(mcpTool{
		Name: "list_env_names", Group: "hosting", Title: "List environment variable names", ReadOnly: true,
		Description: "The names of an app's environment variables, and whether each is marked secret. Values are never returned.",
		InputSchema: object([]string{"project", "app"}, map[string]any{
			"project": str("The project's slug or id."), "app": str("The app's slug or id."),
		}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project, App string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			var vars []struct {
				Key      string `json:"key"`
				IsSecret bool   `json:"isSecret"`
			}
			if err := b.api.get(r.Context(), "/api/projects/"+p.ID+"/apps/"+queryEscape(a.App)+"/env", &vars); err != nil {
				return nil, toolFailure("could not read the environment: %v", err)
			}
			out := make([]map[string]any, 0, len(vars))
			for _, v := range vars {
				out = append(out, map[string]any{"key": v.Key, "isSecret": v.IsSecret})
			}
			return map[string]any{"variables": out}, nil
		},
	})

	register(mcpTool{
		Name: "trigger_deploy", Group: "hosting", Title: "Deploy an app",
		Description: "Build and deploy an app from the latest commit on its " +
			"branch. The previous version keeps serving until the new one " +
			"is healthy. Returns the deploy that was queued, not the " +
			"finished result.",
		InputSchema: object([]string{"project", "app"}, map[string]any{
			"project":   str("The project's slug or id."),
			"app":       str("The app's slug or id."),
			"commitSha": str("Deploy a specific commit instead of the branch head. Optional."),
		}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project, App, CommitSha string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			var deploy any
			body := map[string]any{"commitSha": a.CommitSha}
			if err := b.api.post(r.Context(), "/api/projects/"+p.ID+"/apps/"+queryEscape(a.App)+"/deploys", body, &deploy); err != nil {
				return nil, toolFailure("could not start the deploy: %v", err)
			}
			return deploy, nil
		},
	})

	register(mcpTool{
		Name: "diagnose_app", Group: "hosting", UntrustedData: true, Title: "Diagnose a failing app", ReadOnly: true,
		Description: "Everything needed to explain why an app is not " +
			"working, in one call: its current status, its recent deploys " +
			"and the tail of its logs. Prefer this to calling the " +
			"individual tools one by one.",
		InputSchema: object([]string{"project", "app"}, map[string]any{
			"project": str("The project's slug or id."), "app": str("The app's slug or id."),
		}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project, App string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			var app any
			if err := b.api.get(r.Context(), "/api/projects/"+p.ID+"/apps/"+queryEscape(a.App), &app); err != nil {
				return nil, toolFailure("no app called %q in %s", a.App, p.Slug)
			}
			report := map[string]any{"project": projectSummary(p), "app": app}

			deployRaw, _ := json.Marshal(map[string]any{"project": a.Project, "app": a.App, "limit": 5})
			if deploys, err := b.listDeploys(r, deployRaw); err == nil {
				report["recentDeploys"] = deploys
			} else {
				report["recentDeploys"] = "unavailable"
			}

			var lines any
			logPath := fmt.Sprintf("/api/projects/%s/apps/%s/logs?tail=100", p.ID, queryEscape(a.App))
			if err := b.api.get(r.Context(), logPath, &lines); err == nil {
				report["logs"] = lines
			} else {
				report["logs"] = "unavailable: the app may never have started"
			}
			return report, nil
		},
	})

	// ── Storage ──────────────────────────────────────────────────────────

	register(mcpTool{
		Name: "list_storage_buckets", Group: "storage", ReadOnly: true, UntrustedData: true, Title: "List storage buckets",
		Description: "The storage buckets in a project, and whether each is public.",
		InputSchema: object([]string{"project"}, map[string]any{"project": str("The project's slug or id.")}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			var buckets any
			if err := b.api.get(r.Context(), "/api/projects/"+p.ID+"/storage/buckets", &buckets); err != nil {
				return nil, toolFailure("could not list buckets: %v", err)
			}
			return map[string]any{"buckets": buckets}, nil
		},
	})

	register(mcpTool{
		Name: "list_storage_objects", Group: "storage", ReadOnly: true, UntrustedData: true, Title: "List objects in a bucket",
		Description: "The objects in one storage bucket, optionally under a path prefix.",
		InputSchema: object([]string{"project", "bucket"}, map[string]any{
			"project": str("The project's slug or id."),
			"bucket":  str("The bucket's name or id."),
			"prefix":  str("Only objects whose path starts with this. Optional."),
			"limit":   integer("How many objects to return. Defaults to 100.", 100),
		}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct {
				Project, Bucket, Prefix string
				Limit                   int
			}
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			if a.Limit <= 0 || a.Limit > 500 {
				a.Limit = 100
			}
			var objects any
			path := fmt.Sprintf("/api/projects/%s/storage/buckets/%s/objects?prefix=%s&limit=%d",
				p.ID, queryEscape(a.Bucket), queryEscape(a.Prefix), a.Limit)
			if err := b.api.get(r.Context(), path, &objects); err != nil {
				return nil, toolFailure("could not list objects: %v", err)
			}
			return map[string]any{"objects": objects}, nil
		},
	})

	// ── Edge functions ───────────────────────────────────────────────────

	register(mcpTool{
		Name: "list_edge_functions", Group: "functions", ReadOnly: true, UntrustedData: true, Title: "List edge functions",
		Description: "The edge functions in a project, with each one's status and slug.",
		InputSchema: object([]string{"project"}, map[string]any{"project": str("The project's slug or id.")}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			var functions any
			if err := b.api.get(r.Context(), "/api/projects/"+p.ID+"/functions", &functions); err != nil {
				return nil, toolFailure("could not list functions: %v", err)
			}
			return map[string]any{"functions": functions}, nil
		},
	})

	register(mcpTool{
		Name: "get_edge_function_logs", Group: "functions", ReadOnly: true, UntrustedData: true, Title: "Read an edge function's logs",
		Description: "The most recent output from one edge function.",
		InputSchema: object([]string{"project", "function"}, map[string]any{
			"project":  str("The project's slug or id."),
			"function": str("The function's id."),
			"tail":     integer("How many lines from the end. Defaults to 100.", 100),
		}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct {
				Project, Function string
				Tail              int
			}
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			if a.Tail <= 0 || a.Tail > 1000 {
				a.Tail = 100
			}
			var lines any
			path := fmt.Sprintf("/api/projects/%s/functions/%s/logs?tail=%d", p.ID, queryEscape(a.Function), a.Tail)
			if err := b.api.get(r.Context(), path, &lines); err != nil {
				return nil, toolFailure("could not read the function's logs: %v", err)
			}
			return map[string]any{"lines": lines}, nil
		},
	})

	// ── Branching ────────────────────────────────────────────────────────

	register(mcpTool{
		Name: "list_branches", Group: "branching", ReadOnly: true, UntrustedData: true, Title: "List database branches",
		Description: "The database branches of a project. A branch is a copy of the schema that can be changed without touching the live one.",
		InputSchema: object([]string{"project"}, map[string]any{"project": str("The project's slug or id.")}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			var branches any
			if err := b.api.get(r.Context(), "/api/projects/"+p.ID+"/database/branches", &branches); err != nil {
				return nil, toolFailure("could not list branches: %v", err)
			}
			return map[string]any{"branches": branches}, nil
		},
	})

	// ── Database detail ──────────────────────────────────────────────────

	register(mcpTool{
		Name: "list_extensions", Group: "database", ReadOnly: true, UntrustedData: true, Title: "List installed extensions",
		Description: "The Postgres extensions installed in a project, with their versions.",
		InputSchema: object([]string{"project"}, map[string]any{"project": str("The project's slug or id.")}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			const q = `SELECT e.extname AS name, e.extversion AS version, n.nspname AS schema
				FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace ORDER BY e.extname`
			result, err := b.runReadOnlyQuery(r, p.ID, q, 200)
			if err != nil {
				return nil, toolFailure("could not read the extension list: %v", err)
			}
			return map[string]any{"extensions": result["rows"]}, nil
		},
	})

	register(mcpTool{
		Name: "get_advisors", Group: "database", ReadOnly: true, UntrustedData: true, Title: "Check a project for problems",
		Description: "Look over a project's schema for the mistakes that " +
			"matter: tables readable by anyone because row-level security " +
			"is off, tables with no primary key, and columns that look " +
			"like they hold secrets in plain text.",
		InputSchema: object([]string{"project"}, map[string]any{"project": str("The project's slug or id.")}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			findings := []map[string]any{}

			var rls []rlsRow
			if err := b.api.get(r.Context(), "/api/projects/"+p.ID+"/database/rls", &rls); err == nil {
				for _, row := range rls {
					if row.RLSEnabled || row.Schema != "public" {
						continue
					}
					findings = append(findings, map[string]any{
						"level": "error", "category": "security",
						"table":   row.Schema + "." + row.Table,
						"finding": "Row-level security is off",
						"detail": "The REST API serves this table to anyone holding the " +
							"project's anon key. Enable RLS and add a policy, or the " +
							"table is world-readable.",
					})
				}
			}

			if cols, err := b.introspectColumns(r, p.ID); err == nil {
				findings = append(findings, schemaAdvice(cols)...)
			}

			sort.SliceStable(findings, func(i, j int) bool { return advisorRank(findings[i]) < advisorRank(findings[j]) })

			summary := fmt.Sprintf("%d finding(s)", len(findings))
			if len(findings) == 0 {
				summary = "Nothing to report: every table has row-level security and a primary key, and no column looks like a plaintext secret."
			}
			return map[string]any{"summary": summary, "findings": findings}, nil
		},
	})

	register(mcpTool{
		Name: "generate_typescript_types", Group: "database", ReadOnly: true, UntrustedData: true, Title: "Generate TypeScript types",
		Description: "TypeScript interfaces for a project's tables, generated from the live schema.",
		InputSchema: object([]string{"project"}, map[string]any{"project": str("The project's slug or id.")}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			cols, err := b.introspectColumns(r, p.ID)
			if err != nil {
				return nil, toolFailure("could not read the schema: %v", err)
			}
			tables := tablesByName(cols)
			return map[string]any{
				"language": "typescript", "tableCount": len(tables),
				"source": typescriptForTables(tables),
			}, nil
		},
	})

	register(mcpTool{
		Name: "diagnose_project", Group: "database", ReadOnly: true, UntrustedData: true, Title: "Diagnose a project",
		Description: "Everything needed to explain why a project is not " +
			"working, in one call: whether the database has any tables, " +
			"how much of its plan it has used, and whether its tables are protected.",
		InputSchema: object([]string{"project"}, map[string]any{"project": str("The project's slug or id.")}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			ctx := r.Context()
			report := map[string]any{"project": projectSummary(p)}
			var problems []string

			if p.Status == "paused" {
				problems = append(problems, "The project is paused, so none of its services are running. Resume it from the dashboard.")
			}

			var tables []any
			if err := b.api.get(ctx, "/api/projects/"+p.ID+"/tables", &tables); err == nil {
				report["tableCount"] = len(tables)
				if len(tables) == 0 {
					problems = append(problems, "The database has no tables. If this project was moved here, apply your migrations before expecting any query to work.")
				}
			} else {
				report["tableCount"] = "unavailable"
			}

			var rls []rlsRow
			if err := b.api.get(ctx, "/api/projects/"+p.ID+"/database/rls", &rls); err == nil {
				unprotected := []string{}
				for _, row := range rls {
					if !row.RLSEnabled && row.Schema == "public" {
						unprotected = append(unprotected, row.Schema+"."+row.Table)
					}
				}
				report["tablesWithoutRLS"] = unprotected
				if len(unprotected) > 0 {
					problems = append(problems, fmt.Sprintf("%d table(s) have row-level security off, so the REST API serves them to anyone holding the project's anon key.", len(unprotected)))
				}
			}

			var usage any
			if err := b.api.get(ctx, "/api/projects/"+p.ID+"/usage", &usage); err == nil {
				report["usage"] = usage
			}

			if len(problems) == 0 {
				report["diagnosis"] = "No problems found."
			} else {
				report["problems"] = problems
			}
			return report, nil
		},
	})

	register(mcpTool{
		Name: "apply_migration", Group: "database", Destructive: true, Title: "Apply a database migration",
		Description: "Apply a named SQL migration to a project's database - " +
			"creating tables, adding columns, defining policies. The whole " +
			"script runs in one transaction. Every migration is recorded " +
			"against the project whether it succeeded or not. Use run_sql " +
			"to read; use this to change.",
		InputSchema: object([]string{"project", "name", "sql"}, map[string]any{
			"project": str("The project's slug or id."),
			"name": str("A short name in snake_case describing what this changes, " +
				"such as create_product_table. It is how the migration is identified afterwards."),
			"sql": str("The SQL to run. May contain several statements; all of them apply together or none do."),
		}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Project, Name, SQL string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			p, err := b.resolveProject(r, a.Project)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(a.Name) == "" {
				return nil, toolFailure("give the migration a name, so it can be identified later")
			}
			if strings.TrimSpace(a.SQL) == "" {
				return nil, toolFailure("the migration is empty")
			}
			if schema, ok := touchesPlatformSchema(a.SQL); ok {
				return nil, toolFailure("this migration touches the %q schema, which belongs to the platform rather than to your application. Create your tables in public instead.", schema)
			}
			var migration map[string]any
			body := map[string]any{"name": a.Name, "sql": a.SQL}
			if err := b.api.post(r.Context(), "/api/projects/"+p.ID+"/database/migrations", body, &migration); err != nil {
				return nil, toolFailure("%v", err)
			}
			migration["note"] = "Applied in a single transaction. Call list_tables to see the result, and get_advisors if this added tables that need row-level security."
			return migration, nil
		},
	})

	// ── Search and fetch ─────────────────────────────────────────────────

	register(mcpTool{
		Name: "search", Group: "core", ReadOnly: true, UntrustedData: true, Title: "Search Afribase",
		Description: "Find projects and deployed apps by name. Returns a list of results, each with an id that fetch will expand.",
		InputSchema: object([]string{"query"}, map[string]any{"query": str("What to look for. Matched against project and app names and slugs.")}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ Query string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			needle := strings.ToLower(strings.TrimSpace(a.Query))
			var projects []project
			if err := b.api.get(r.Context(), "/api/projects", &projects); err != nil {
				return nil, err
			}
			results := []map[string]any{}
			for i := range projects {
				p := &projects[i]
				if matches(needle, p.Name, p.Slug, p.Description) {
					results = append(results, map[string]any{
						"id": "project:" + p.Slug, "title": p.Name,
						"text": fmt.Sprintf("Afribase %s project, status %s", p.Kind, p.Status),
					})
				}
				var apps []struct{ Name, Slug, Status, Repo, Hostname string }
				if err := b.api.get(r.Context(), "/api/projects/"+p.ID+"/apps", &apps); err != nil {
					continue
				}
				for _, app := range apps {
					if !matches(needle, app.Name, app.Slug, app.Repo) {
						continue
					}
					entry := map[string]any{
						"id": "app:" + p.Slug + "/" + app.Slug, "title": app.Name,
						"text": fmt.Sprintf("App in %s, status %s", p.Slug, app.Status),
					}
					if app.Hostname != "" {
						entry["url"] = "https://" + app.Hostname
					}
					results = append(results, entry)
				}
			}
			return map[string]any{"results": results}, nil
		},
	})

	register(mcpTool{
		Name: "fetch", Group: "core", ReadOnly: true, UntrustedData: true, Title: "Fetch a search result",
		Description: "Expand an id returned by search into the full record.",
		InputSchema: object([]string{"id"}, map[string]any{"id": str("An id from search, such as project:my-shop or app:my-shop/api.")}),
		Handler: func(b *Bridge, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct{ ID string }
			if err := mcpArgs(raw, &a); err != nil {
				return nil, err
			}
			kind, rest, ok := strings.Cut(a.ID, ":")
			if !ok {
				return nil, toolFailure("ids look like project:<slug> or app:<project>/<app>")
			}
			switch kind {
			case "project":
				p, err := b.resolveProject(r, rest)
				if err != nil {
					return nil, err
				}
				out := projectSummary(p)
				var usage any
				if err := b.api.get(r.Context(), "/api/projects/"+p.ID+"/usage", &usage); err == nil {
					out["usage"] = usage
				}
				return out, nil
			case "app":
				projectSlug, appSlug, ok := strings.Cut(rest, "/")
				if !ok {
					return nil, toolFailure("an app id looks like app:<project>/<app>")
				}
				args, _ := json.Marshal(map[string]string{"project": projectSlug, "app": appSlug})
				return mcpTools["diagnose_app"].Handler(b, r, args)
			default:
				return nil, toolFailure("unknown id kind %q", kind)
			}
		},
	})
}

// listDeploys is shared by list_deploys and diagnose_app.
func (b *Bridge) listDeploys(r *http.Request, raw json.RawMessage) (any, error) {
	var a struct {
		Project, App string
		Limit        int
	}
	if err := mcpArgs(raw, &a); err != nil {
		return nil, err
	}
	p, err := b.resolveProject(r, a.Project)
	if err != nil {
		return nil, err
	}
	if a.Limit <= 0 || a.Limit > 50 {
		a.Limit = 10
	}
	var deploys any
	path := fmt.Sprintf("/api/projects/%s/apps/%s/deploys?limit=%d", p.ID, queryEscape(a.App), a.Limit)
	if err := b.api.get(r.Context(), path, &deploys); err != nil {
		return nil, toolFailure("could not list deploys: %v", err)
	}
	return deploys, nil
}

func matches(needle string, fields ...string) bool {
	if needle == "" {
		return true
	}
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), needle) {
			return true
		}
	}
	return false
}

// runReadOnlyQuery fences a caller-supplied query and runs it via the SQL
// endpoint. The statement is wrapped in a subselect with a LIMIT, which
// caps the rows and rejects anything that is not a single expression - a
// trailing `; UPDATE ...` becomes a syntax error rather than a second
// statement.
func (b *Bridge) runReadOnlyQuery(r *http.Request, projectID, query string, limit int) (map[string]any, error) {
	wrapped, err := readOnlyQuery(query, limit)
	if err != nil {
		return nil, err
	}
	var result struct {
		Results []map[string]any `json:"results"`
	}
	if err := b.api.post(r.Context(), "/api/projects/"+projectID+"/sql", map[string]string{"query": wrapped}, &result); err != nil {
		return nil, toolFailure("the query failed: %v", err)
	}
	columns := []string{}
	if len(result.Results) > 0 {
		for k := range result.Results[0] {
			columns = append(columns, k)
		}
		sort.Strings(columns)
	}
	return map[string]any{"columns": columns, "rows": result.Results, "rowCount": len(result.Results)}, nil
}

func readOnlyQuery(query string, limit int) (string, error) {
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(query), ";"))
	if trimmed == "" {
		return "", toolFailure("no query was given")
	}
	if strings.Contains(trimmed, ";") {
		return "", toolFailure("send one statement at a time")
	}
	upper := strings.ToUpper(trimmed)
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		return "", toolFailure("this tool runs read-only queries; start with SELECT or WITH")
	}
	for _, forbidden := range []string{"INSERT", "UPDATE", "DELETE", "DROP", "ALTER", "CREATE", "TRUNCATE", "GRANT", "REVOKE"} {
		if containsWord(upper, forbidden) {
			return "", toolFailure("this tool runs read-only queries; %s is not allowed", forbidden)
		}
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return fmt.Sprintf("SELECT * FROM (%s) AS afribase_mcp_query LIMIT %d", trimmed, limit), nil
}

func containsWord(haystack, word string) bool {
	for i := 0; i+len(word) <= len(haystack); i++ {
		if haystack[i:i+len(word)] != word {
			continue
		}
		if i > 0 && isWordByte(haystack[i-1]) {
			continue
		}
		if i+len(word) < len(haystack) && isWordByte(haystack[i+len(word)]) {
			continue
		}
		return true
	}
	return false
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func generatedPassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("could not generate a password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ── Platform schema guard ────────────────────────────────────────────────

var platformSchemas = []string{
	"auth", "storage", "realtime", "_realtime", "vault", "extensions",
	"graphql", "graphql_public", "pgbouncer", "supabase_functions",
	"pg_catalog", "information_schema", "cron", "net",
}

func touchesPlatformSchema(script string) (string, bool) {
	lowered := strings.ToLower(script)
	for _, schema := range platformSchemas {
		for _, form := range []string{schema + ".", `"` + schema + `".`} {
			if idx := strings.Index(lowered, form); idx >= 0 {
				if idx == 0 || !isWordByte(lowered[idx-1]) {
					return schema, true
				}
			}
		}
		for _, verb := range []string{"drop schema ", "alter schema ", "create schema "} {
			if strings.Contains(lowered, verb+schema) ||
				strings.Contains(lowered, verb+`"`+schema+`"`) ||
				strings.Contains(lowered, verb+"if exists "+schema) {
				return schema, true
			}
		}
	}
	return "", false
}

// ── Schema introspection (via information_schema, not an internal call) ──

type rlsRow struct {
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	RLSEnabled bool   `json:"rlsEnabled"`
}

type column struct {
	Schema     string
	Table      string
	Name       string
	Type       string
	Nullable   bool
	PrimaryKey bool
}

// introspectColumns reads every public-schema column, its type, and
// whether it is part of a primary key, over the same read-only SQL path
// run_sql uses. This is what Afribase's own hosted connector gets from an
// internal schema service; a standalone bridge gets it from the database
// directly, since it is exactly what information_schema is for.
func (b *Bridge) introspectColumns(r *http.Request, projectID string) ([]column, error) {
	const q = `SELECT c.table_schema, c.table_name, c.column_name, c.data_type,
		       c.is_nullable = 'YES' AS nullable,
		       COALESCE(pk.is_pk, false) AS is_primary_key
		  FROM information_schema.columns c
		  LEFT JOIN (
		    SELECT ku.table_schema, ku.table_name, ku.column_name, true AS is_pk
		      FROM information_schema.table_constraints tc
		      JOIN information_schema.key_column_usage ku
		        ON tc.constraint_name = ku.constraint_name AND tc.table_schema = ku.table_schema
		     WHERE tc.constraint_type = 'PRIMARY KEY'
		  ) pk ON pk.table_schema = c.table_schema AND pk.table_name = c.table_name AND pk.column_name = c.column_name
		 WHERE c.table_schema = 'public'
		 ORDER BY c.table_name, c.ordinal_position`

	result, err := b.runReadOnlyQuery(r, projectID, q, 5000)
	if err != nil {
		return nil, err
	}
	rows, _ := result["rows"].([]map[string]any)
	out := make([]column, 0, len(rows))
	for _, row := range rows {
		out = append(out, column{
			Schema:     fmt.Sprint(row["table_schema"]),
			Table:      fmt.Sprint(row["table_name"]),
			Name:       fmt.Sprint(row["column_name"]),
			Type:       fmt.Sprint(row["data_type"]),
			Nullable:   fmt.Sprint(row["nullable"]) == "true",
			PrimaryKey: fmt.Sprint(row["is_primary_key"]) == "true",
		})
	}
	return out, nil
}

func tablesByName(cols []column) map[string][]column {
	out := map[string][]column{}
	for _, c := range cols {
		key := c.Schema + "." + c.Table
		out[key] = append(out[key], c)
	}
	return out
}

var secretishColumn = map[string]bool{
	"password": true, "passwd": true, "secret": true, "token": true,
	"api_key": true, "apikey": true, "private_key": true, "credit_card": true,
	"card_number": true, "cvv": true, "ssn": true,
}

func schemaAdvice(cols []column) []map[string]any {
	findings := []map[string]any{}
	for table, tableCols := range tablesByName(cols) {
		hasPrimaryKey := false
		for _, col := range tableCols {
			if col.PrimaryKey {
				hasPrimaryKey = true
			}
			name := strings.ToLower(col.Name)
			if !secretishColumn[name] {
				continue
			}
			if strings.Contains(name, "hash") || strings.Contains(name, "digest") {
				continue
			}
			findings = append(findings, map[string]any{
				"level": "warning", "category": "security", "table": table, "column": col.Name,
				"finding": "This column looks like it holds a secret",
				"detail": "If it stores a password, store a hash instead. If it stores " +
					"a key or a card number, it belongs in the vault, not in a column " +
					"that every backup and every query plan can see.",
			})
		}
		if !hasPrimaryKey {
			findings = append(findings, map[string]any{
				"level": "warning", "category": "correctness", "table": table,
				"finding": "The table has no primary key",
				"detail": "Rows cannot be addressed individually, so the REST API " +
					"cannot update or delete one, and duplicates cannot be prevented.",
			})
		}
	}
	return findings
}

func advisorRank(finding map[string]any) int {
	if finding["level"] == "error" {
		return 0
	}
	return 1
}

func typescriptForTables(tables map[string][]column) string {
	var b strings.Builder
	b.WriteString("// Generated from the live Afribase schema.\n")
	b.WriteString("// Regenerate after changing a table; do not edit by hand.\n\n")

	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, qualified := range names {
		parts := strings.SplitN(qualified, ".", 2)
		name := parts[len(parts)-1]
		fmt.Fprintf(&b, "export interface %s {\n", pascalCase(name))
		for _, col := range tables[qualified] {
			tsType := typescriptType(col.Type)
			if col.Nullable {
				tsType += " | null"
			}
			fmt.Fprintf(&b, "  %s: %s;\n", quotedKey(col.Name), tsType)
		}
		b.WriteString("}\n\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func typescriptType(pgType string) string {
	t := strings.ToLower(strings.TrimSpace(pgType))
	if idx := strings.Index(t, "("); idx > 0 {
		t = strings.TrimSpace(t[:idx])
	}
	if strings.HasSuffix(t, "[]") {
		return typescriptType(strings.TrimSuffix(t, "[]")) + "[]"
	}
	switch t {
	case "bool", "boolean":
		return "boolean"
	case "smallint", "int2", "integer", "int", "int4", "real", "float4", "double precision", "float8":
		return "number"
	case "bigint", "int8", "numeric", "decimal", "money":
		return "string"
	case "json", "jsonb":
		return "unknown"
	case "uuid", "text", "varchar", "character varying", "char", "character",
		"date", "time", "timetz", "timestamp", "timestamptz",
		"timestamp with time zone", "timestamp without time zone",
		"bytea", "inet", "cidr", "macaddr", "interval":
		return "string"
	default:
		return "string"
	}
}

func quotedKey(name string) string {
	plain := name != ""
	for i := 0; i < len(name); i++ {
		ch := name[i]
		isLetter := ch == '_' || ch == '$' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
		isDigit := ch >= '0' && ch <= '9'
		if !isLetter && !(isDigit && i > 0) {
			plain = false
			break
		}
	}
	if plain {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `\"`) + `"`
}

func pascalCase(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '_' || r == '-' || r == ' ' || r == '.'
	})
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]))
		b.WriteString(part[1:])
	}
	if b.Len() == 0 {
		return "Row"
	}
	return b.String()
}
