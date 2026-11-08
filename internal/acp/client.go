package acp

import (
	"context"
	"log/slog"

	sdk "github.com/coder/acp-go-sdk"
)

// client is the ACP client side the runtime calls back into. The gateway
// advertises no file system or terminal capability, so the runtime's own
// side-effect tools have nothing to act on through it.
type client struct{ r *Runtime }

var _ sdk.Client = (*client)(nil)

func (c *client) SessionUpdate(_ context.Context, n sdk.SessionNotification) error {
	if c.r.opt.OnUpdate != nil {
		c.r.opt.OnUpdate(n)
	}
	return nil
}

// RequestPermission approves only MCP calls into the gateway's own tool
// bridge and rejects every other tool the runtime wants to run.
func (c *client) RequestPermission(_ context.Context, p sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	allowed := isOwnMCPCall(p.ToolCall.Meta, c.r.opt.MCPServerName)
	want := sdk.PermissionOptionKindRejectOnce
	if allowed {
		want = sdk.PermissionOptionKindAllowOnce
	}
	for _, o := range p.Options {
		if o.Kind == want {
			return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeSelected(o.OptionId)}, nil
		}
	}
	if !allowed {
		title := ""
		if p.ToolCall.Title != nil {
			title = *p.ToolCall.Title
		}
		c.r.log.Info("rejected runtime tool", slog.String("session", string(p.SessionId)), slog.String("tool", title))
	}
	return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeCancelled()}, nil
}

// IsOwnMCPCall reports whether tool call metadata names the gateway's MCP
// server. Exported for the session layer, which hides these internal calls.
func IsOwnMCPCall(meta map[string]any, server string) bool { return isOwnMCPCall(meta, server) }

func isOwnMCPCall(meta map[string]any, server string) bool {
	isMCP, _ := meta["is_mcp_tool_call"].(bool)
	name, _ := dig(meta, "mcp", "server").(string)
	return isMCP && name == server
}

func (*client) ReadTextFile(context.Context, sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	return sdk.ReadTextFileResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodFsReadTextFile)
}

func (*client) WriteTextFile(context.Context, sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	return sdk.WriteTextFileResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodFsWriteTextFile)
}

func (*client) CreateTerminal(context.Context, sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	return sdk.CreateTerminalResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalCreate)
}

func (*client) KillTerminal(context.Context, sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	return sdk.KillTerminalResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalKill)
}

func (*client) TerminalOutput(context.Context, sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	return sdk.TerminalOutputResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalOutput)
}

func (*client) ReleaseTerminal(context.Context, sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	return sdk.ReleaseTerminalResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalRelease)
}

func (*client) WaitForTerminalExit(context.Context, sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	return sdk.WaitForTerminalExitResponse{}, sdk.NewMethodNotFound(sdk.ClientMethodTerminalWaitForExit)
}
