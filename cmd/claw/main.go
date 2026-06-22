// Command claw is the kube-claw control-plane CLI (DESIGN.md §14). It talks to
// the controller API; for local use, port-forward the controller and pass
// --controller-url (or set CLAW_CONTROLLER_URL).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var controllerURL string

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{Use: "claw", Short: "kube-claw control-plane CLI", SilenceUsage: true, SilenceErrors: true}
	def := os.Getenv("CLAW_CONTROLLER_URL")
	if def == "" {
		def = "http://localhost:8443"
	}
	root.PersistentFlags().StringVar(&controllerURL, "controller-url", def, "controller API base URL")
	root.AddCommand(newSecretCmd(), newRunCmd(), newRunsCmd(), newAgentsCmd(), newBaseImageCmd(), newPromptCmd())
	return root
}

func newSecretCmd() *cobra.Command {
	c := &cobra.Command{Use: "secret", Short: "Manage secrets"}

	var ns, typ, description string
	var granters []string
	create := &cobra.Command{
		Use:   "create NAME",
		Short: "Create secret metadata; prints a one-time intake link",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var out map[string]string
			if err := apiJSON(http.MethodPost, "/v1/secrets", map[string]any{
				"namespace": ns, "name": args[0], "type": typ, "description": description, "granters": granters,
			}, &out); err != nil {
				return err
			}
			fmt.Printf("created secret %s\n", out["id"])
			fmt.Printf("open this one-time link to submit the value:\n  %s\n", out["intakeURL"])
			return nil
		},
	}
	create.Flags().StringVar(&ns, "namespace", "claw-agents", "namespace")
	create.Flags().StringVar(&typ, "type", "", "secret type")
	create.Flags().StringVar(&description, "description", "", "what the secret is / how the agent should use it")
	create.Flags().StringArrayVar(&granters, "granter", nil, "granter principal (repeatable)")

	var putNS, fromFile string
	put := &cobra.Command{
		Use:   "put NAME",
		Short: "Upload a value (break-glass / CI)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			data, err := os.ReadFile(fromFile)
			if err != nil {
				return err
			}
			return apiRaw(http.MethodPut, "/v1/secrets/"+args[0]+"/versions?namespace="+putNS, data)
		},
	}
	put.Flags().StringVar(&putNS, "namespace", "claw-agents", "namespace")
	put.Flags().StringVar(&fromFile, "from-file", "", "file containing the secret value")
	_ = put.MarkFlagRequired("from-file")

	var metaNS string
	meta := &cobra.Command{
		Use:   "metadata NAME",
		Short: "Show secret metadata (never the value)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiPrint(http.MethodGet, "/v1/secrets/"+args[0]+"/metadata?namespace="+metaNS)
		},
	}
	meta.Flags().StringVar(&metaNS, "namespace", "claw-agents", "namespace")

	// requests list
	var reqStatus string
	requests := &cobra.Command{Use: "requests", Short: "List secret approval requests", RunE: func(_ *cobra.Command, _ []string) error {
		q := "/v1/secret-requests"
		if reqStatus != "" {
			q += "?status=" + reqStatus
		}
		return apiPrint(http.MethodGet, q)
	}}
	requests.Flags().StringVar(&reqStatus, "status", "Pending", "filter by status (empty = all)")

	var approver, reason string
	approve := &cobra.Command{Use: "approve REQUEST_ID", Short: "Approve a request (break-glass)", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiJSON(http.MethodPost, "/v1/secret-requests/"+args[0]+"/approve",
				map[string]any{"approver": approver, "reason": reason}, nil)
		}}
	approve.Flags().StringVar(&approver, "approver", "cli", "approver principal")
	approve.Flags().StringVar(&reason, "reason", "", "reason")

	var denyReason string
	deny := &cobra.Command{Use: "deny REQUEST_ID", Short: "Deny a request", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiJSON(http.MethodPost, "/v1/secret-requests/"+args[0]+"/deny",
				map[string]any{"approver": "cli", "reason": denyReason}, nil)
		}}
	deny.Flags().StringVar(&denyReason, "reason", "", "reason")

	// grants
	var grantsNS, grantsAgent string
	grants := &cobra.Command{Use: "grants", Short: "List grants", RunE: func(_ *cobra.Command, _ []string) error {
		return apiPrint(http.MethodGet, "/v1/secret-grants?namespace="+grantsNS+"&agent="+grantsAgent)
	}}
	grants.Flags().StringVar(&grantsNS, "namespace", "claw-agents", "namespace")
	grants.Flags().StringVar(&grantsAgent, "agent", "", "agent name")

	var revokeReason string
	grant := &cobra.Command{Use: "grant", Short: "Manage grants"}
	revoke := &cobra.Command{Use: "revoke GRANT_ID", Short: "Revoke a grant", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiJSON(http.MethodPost, "/v1/secret-grants/"+args[0]+"/revoke",
				map[string]any{"approver": "cli", "reason": revokeReason}, nil)
		}}
	revoke.Flags().StringVar(&revokeReason, "reason", "", "reason")
	grant.AddCommand(revoke)

	c.AddCommand(create, put, meta, requests, approve, deny, grants, grant)
	return c
}

func newRunCmd() *cobra.Command {
	c := &cobra.Command{Use: "run", Short: "Trigger runs"}
	var ns, agent, input string
	create := &cobra.Command{
		Use:   "create",
		Short: "Trigger a run directly (no Slack)",
		RunE: func(_ *cobra.Command, _ []string) error {
			var out map[string]string
			if err := apiJSON(http.MethodPost, "/v1/runs", map[string]any{
				"namespace": ns, "agent": agent, "input": input,
			}, &out); err != nil {
				return err
			}
			fmt.Printf("run %s (%s)\n", out["id"], out["phase"])
			return nil
		},
	}
	create.Flags().StringVar(&ns, "namespace", "claw-agents", "namespace")
	create.Flags().StringVar(&agent, "agent", "", "agent name")
	create.Flags().StringVar(&input, "input", "", "input text")
	_ = create.MarkFlagRequired("agent")
	c.AddCommand(create)
	return c
}

func newRunsCmd() *cobra.Command {
	c := &cobra.Command{Use: "runs", Short: "Inspect runs"}
	c.AddCommand(
		&cobra.Command{Use: "list", Short: "List runs", RunE: func(_ *cobra.Command, _ []string) error {
			return apiPrint(http.MethodGet, "/v1/runs")
		}},
		&cobra.Command{Use: "show RUN_ID", Short: "Show a run", Args: cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				return apiPrint(http.MethodGet, "/v1/runs/"+args[0])
			}},
	)
	return c
}

func newBaseImageCmd() *cobra.Command {
	c := &cobra.Command{Use: "baseimage", Short: "Manage base images"}
	var image, description string
	create := &cobra.Command{
		Use: "create NAME", Short: "Register a base image (with a 'when to use' description)", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiJSON(http.MethodPost, "/v1/base-images",
				map[string]any{"name": args[0], "image": image, "description": description}, nil)
		},
	}
	create.Flags().StringVar(&image, "image", "", "container image (digest-pinned for prod)")
	create.Flags().StringVar(&description, "description", "", "when to use this base image")
	_ = create.MarkFlagRequired("image")
	c.AddCommand(create, &cobra.Command{Use: "list", Short: "List base images", RunE: func(_ *cobra.Command, _ []string) error {
		return apiPrint(http.MethodGet, "/v1/base-images")
	}})
	return c
}

func newAgentsCmd() *cobra.Command {
	c := &cobra.Command{Use: "agents", Aliases: []string{"agent"}, Short: "Manage agents"}
	c.AddCommand(&cobra.Command{Use: "list", Short: "List agents", RunE: func(_ *cobra.Command, _ []string) error {
		return apiPrint(http.MethodGet, "/v1/agents")
	}})

	var ns, base, image, prompt, idle string
	var secretSpecs []string
	create := &cobra.Command{
		Use:   "create NAME",
		Short: "Register an agent (no YAML; the controller creates the Agent CRD)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			secrets := make([]map[string]string, 0, len(secretSpecs))
			for _, s := range secretSpecs {
				// format: name[:path[:ENVVAR]]
				p := strings.SplitN(s, ":", 3)
				m := map[string]string{"name": p[0]}
				if len(p) > 1 {
					m["path"] = p[1]
				}
				if len(p) > 2 {
					m["env"] = p[2]
				}
				secrets = append(secrets, m)
			}
			return apiJSON(http.MethodPost, "/v1/agents", map[string]any{
				"namespace": ns, "name": args[0], "baseImageRef": base, "image": image,
				"systemPrompt": prompt, "idleTimeout": idle, "secrets": secrets,
			}, nil)
		},
	}
	create.Flags().StringVar(&ns, "namespace", "claw-agents", "namespace")
	create.Flags().StringVar(&base, "base", "", "base image ref (registered base image name)")
	create.Flags().StringVar(&image, "image", "", "explicit digest-pinned image (alternative to --base)")
	create.Flags().StringVar(&prompt, "system-prompt", "", "agent system prompt")
	create.Flags().StringVar(&idle, "idle-timeout", "15m", "scale-to-zero idle timeout")
	create.Flags().StringArrayVar(&secretSpecs, "secret", nil, "secret as name:path:ENVVAR (repeatable)")
	c.AddCommand(create)
	return c
}

func newPromptCmd() *cobra.Command {
	c := &cobra.Command{Use: "prompt", Short: "Manage editable agent system prompts"}
	c.AddCommand(&cobra.Command{Use: "list", Short: "List prompts", RunE: func(_ *cobra.Command, _ []string) error {
		return apiPrint(http.MethodGet, "/v1/prompts")
	}})
	c.AddCommand(&cobra.Command{Use: "get NAMESPACE NAME", Args: cobra.ExactArgs(2), Short: "Get an agent's prompt",
		RunE: func(_ *cobra.Command, a []string) error {
			return apiPrint(http.MethodGet, "/v1/prompts/"+a[0]+"/"+a[1])
		}})
	var ns string
	set := &cobra.Command{Use: "set NAME CONTENT", Args: cobra.ExactArgs(2), Short: "Set an agent's prompt",
		RunE: func(_ *cobra.Command, a []string) error {
			return apiJSON(http.MethodPut, "/v1/prompts", map[string]any{"namespace": ns, "name": a[0], "content": a[1]}, nil)
		}}
	set.Flags().StringVar(&ns, "namespace", "claw-agents", "namespace")
	c.AddCommand(set)
	return c
}

// --- tiny API client ---

func httpClient() *http.Client { return &http.Client{Timeout: 15 * time.Second} }

func apiJSON(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, controllerURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, string(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func apiRaw(method, path string, body []byte) error {
	req, err := http.NewRequest(method, controllerURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, string(data))
	}
	fmt.Println(string(data))
	return nil
}

func apiPrint(method, path string) error {
	req, _ := http.NewRequest(method, controllerURL+path, nil)
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, string(data))
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, data, "", "  ") == nil {
		fmt.Println(pretty.String())
	} else {
		fmt.Println(string(data))
	}
	return nil
}
