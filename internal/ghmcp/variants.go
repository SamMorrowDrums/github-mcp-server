package ghmcp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"

	gherrors "github.com/github/github-mcp-server/pkg/errors"
	"github.com/github/github-mcp-server/pkg/github"
	"github.com/github/github-mcp-server/pkg/inventory"
	mcplog "github.com/github/github-mcp-server/pkg/log"
	"github.com/github/github-mcp-server/pkg/octicons"
	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/github/github-mcp-server/pkg/utils"
	"github.com/modelcontextprotocol/experimental-ext-variants/go/sdk/variants"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewVariantsStdioMCPServer creates a variants.Server where each toolset is exposed
// as a separate variant. This replaces toolset configuration with the MCP variants
// extension (SEP-2053), allowing clients to dynamically select which toolset to use
// per-request via _meta.
func NewVariantsStdioMCPServer(ctx context.Context, cfg github.MCPServerConfig) (*variants.Server, error) {
	apiHost, err := utils.NewAPIHost(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("failed to parse API host: %w", err)
	}

	clients, err := createGitHubClients(cfg, apiHost)
	if err != nil {
		return nil, fmt.Errorf("failed to create GitHub clients: %w", err)
	}

	featureChecker := createFeatureChecker(cfg.EnabledFeatures)

	deps := github.NewBaseDeps(
		clients.rest,
		clients.gql,
		clients.raw,
		clients.repoAccess,
		cfg.Translator,
		github.FeatureFlags{
			LockdownMode: cfg.LockdownMode,
			InsidersMode: cfg.InsidersMode,
		},
		cfg.ContentWindowSize,
		featureChecker,
	)

	// Build a full inventory with ALL toolsets enabled (no filtering).
	// Each toolset will become its own variant.
	inv, err := github.NewInventory(cfg.Translator).
		WithDeprecatedAliases(github.DeprecatedToolAliases).
		WithReadOnly(cfg.ReadOnly).
		WithToolsets([]string{"all"}).
		WithServerInstructions().
		WithFeatureChecker(featureChecker).
		WithInsidersMode(cfg.InsidersMode).
		Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build inventory: %w", err)
	}

	// Group tools, resources, and prompts by toolset
	toolsByToolset := groupToolsByToolset(inv.AvailableTools(ctx))
	resourcesByToolset := groupResourcesByToolset(inv.AvailableResourceTemplates(ctx))
	promptsByToolset := groupPromptsByToolset(inv.AvailablePrompts(ctx))

	// Collect all unique toolset IDs from tools, resources, and prompts
	allToolsetIDs := collectToolsetIDs(toolsByToolset, resourcesByToolset, promptsByToolset)

	impl := &mcp.Implementation{
		Name:    "github-mcp-server",
		Title:   "GitHub MCP Server",
		Version: cfg.Version,
		Icons:   octicons.Icons("mark-github"),
	}

	vs := variants.NewServer(impl)

	// Default toolsets get lower priority values (higher priority)
	defaultPriority := 0
	nonDefaultPriority := 100

	for _, toolsetID := range allToolsetIDs {
		tools := toolsByToolset[toolsetID]
		resources := resourcesByToolset[toolsetID]
		prompts := promptsByToolset[toolsetID]

		if len(tools) == 0 && len(resources) == 0 && len(prompts) == 0 {
			continue
		}

		// Determine priority and status from the first tool's toolset metadata
		var meta inventory.ToolsetMetadata
		switch {
		case len(tools) > 0:
			meta = tools[0].Toolset
		case len(resources) > 0:
			meta = resources[0].Toolset
		case len(prompts) > 0:
			meta = prompts[0].Toolset
		}

		priority := nonDefaultPriority
		if meta.Default {
			priority = defaultPriority
			defaultPriority++
		} else {
			nonDefaultPriority++
		}

		// Create per-toolset server
		serverOpts := &mcp.ServerOptions{
			Logger: cfg.Logger,
		}
		toolsetServer := mcp.NewServer(impl, serverOpts)

		// Add middleware for deps injection and error context
		toolsetServer.AddReceivingMiddleware(github.InjectDepsMiddleware(deps))
		toolsetServer.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				ctx = gherrors.ContextWithGitHubErrors(ctx)
				return next(ctx, method, req)
			}
		})

		// Register tools
		for i := range tools {
			tools[i].RegisterFunc(toolsetServer, deps)
		}

		// Register resources
		for i := range resources {
			templateCopy := resources[i].Template
			if len(templateCopy.Icons) == 0 {
				templateCopy.Icons = resources[i].Toolset.Icons()
			}
			toolsetServer.AddResourceTemplate(&templateCopy, resources[i].Handler(deps))
		}

		// Register prompts
		for i := range prompts {
			promptCopy := prompts[i].Prompt
			if len(promptCopy.Icons) == 0 {
				promptCopy.Icons = prompts[i].Toolset.Icons()
			}
			toolsetServer.AddPrompt(&promptCopy, prompts[i].Handler)
		}

		// Build hints from toolset metadata
		hints := map[string]string{
			"toolset": string(meta.ID),
		}
		if meta.Default {
			hints["default"] = "true"
		}

		vs = vs.WithVariant(variants.ServerVariant{
			ID:          string(meta.ID),
			Description: meta.Description,
			Status:      variants.Stable,
			Hints:       hints,
		}, toolsetServer, priority)
	}

	// Custom ranking: boost variants whose toolset ID matches the client's hint
	vs = vs.WithRanking(func(_ context.Context, hints variants.VariantHints, vs []variants.ServerVariant) []variants.ServerVariant {
		requested, _ := variants.HintValue[string](hints, "toolset")
		slices.SortStableFunc(vs, func(a, b variants.ServerVariant) int {
			aMatch := strings.EqualFold(a.Hints["toolset"], requested)
			bMatch := strings.EqualFold(b.Hints["toolset"], requested)
			if aMatch != bMatch {
				if aMatch {
					return -1
				}
				return 1
			}
			return a.Priority() - b.Priority()
		})
		return vs
	})

	return vs, nil
}

// groupToolsByToolset groups tools by their toolset ID.
func groupToolsByToolset(tools []inventory.ServerTool) map[inventory.ToolsetID][]inventory.ServerTool {
	result := make(map[inventory.ToolsetID][]inventory.ServerTool)
	for _, tool := range tools {
		result[tool.Toolset.ID] = append(result[tool.Toolset.ID], tool)
	}
	return result
}

// groupResourcesByToolset groups resource templates by their toolset ID.
func groupResourcesByToolset(resources []inventory.ServerResourceTemplate) map[inventory.ToolsetID][]inventory.ServerResourceTemplate {
	result := make(map[inventory.ToolsetID][]inventory.ServerResourceTemplate)
	for _, res := range resources {
		result[res.Toolset.ID] = append(result[res.Toolset.ID], res)
	}
	return result
}

// groupPromptsByToolset groups prompts by their toolset ID.
func groupPromptsByToolset(prompts []inventory.ServerPrompt) map[inventory.ToolsetID][]inventory.ServerPrompt {
	result := make(map[inventory.ToolsetID][]inventory.ServerPrompt)
	for _, prompt := range prompts {
		result[prompt.Toolset.ID] = append(result[prompt.Toolset.ID], prompt)
	}
	return result
}

// collectToolsetIDs returns sorted unique toolset IDs from all item maps.
func collectToolsetIDs(
	tools map[inventory.ToolsetID][]inventory.ServerTool,
	resources map[inventory.ToolsetID][]inventory.ServerResourceTemplate,
	prompts map[inventory.ToolsetID][]inventory.ServerPrompt,
) []inventory.ToolsetID {
	idSet := make(map[inventory.ToolsetID]bool)
	for id := range tools {
		idSet[id] = true
	}
	for id := range resources {
		idSet[id] = true
	}
	for id := range prompts {
		idSet[id] = true
	}

	ids := make([]inventory.ToolsetID, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// runVariantsStdioServer creates and runs a variants-based MCP server over stdio.
func runVariantsStdioServer(
	ctx context.Context,
	mcpCfg github.MCPServerConfig,
	cfg StdioServerConfig,
	logger *slog.Logger,
	t translations.TranslationHelperFunc,
	dumpTranslations func(),
) error {
	vs, err := NewVariantsStdioMCPServer(ctx, mcpCfg)
	if err != nil {
		return fmt.Errorf("failed to create variants MCP server: %w", err)
	}
	defer vs.Close()

	if cfg.ExportTranslations {
		dumpTranslations()
	}

	logger.Info("using variants mode - each toolset is a separate variant",
		"variantCount", len(vs.Variants()))
	for _, v := range vs.Variants() {
		logger.Info("registered variant", "id", v.ID, "description", v.Description, "priority", v.Priority())
	}

	errC := make(chan error, 1)
	go func() {
		var in io.ReadCloser
		var out io.WriteCloser

		in = os.Stdin
		out = os.Stdout

		if cfg.EnableCommandLogging {
			loggedIO := mcplog.NewIOLogger(in, out, logger)
			in, out = loggedIO, loggedIO
		}

		_ = t // translations already applied via mcpCfg.Translator
		errC <- vs.Run(ctx, &mcp.IOTransport{Reader: in, Writer: out})
	}()

	_, _ = fmt.Fprintf(os.Stderr, "GitHub MCP Server running on stdio (variants mode)\n")

	select {
	case <-ctx.Done():
		logger.Info("shutting down server", "signal", "context done")
	case err := <-errC:
		if err != nil {
			logger.Error("error running server", "error", err)
			return fmt.Errorf("error running server: %w", err)
		}
	}

	return nil
}
