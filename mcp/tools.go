package mcp

import (
	"github.com/mark3labs/mcp-go/mcp"
)

// ToolDefinitions returns all MCP tool schemas exposed by VoidRun.
func ToolDefinitions() []mcp.Tool {
	return []mcp.Tool{
		toolCreateSandbox(),
		toolListSandboxes(),
		toolGetSandbox(),
		toolDeleteSandbox(),
		toolExecuteCommand(),
		toolReadFile(),
		toolWriteFile(),
		toolListFiles(),
		toolCreateDirectory(),
		toolDeleteFile(),
		toolMoveFile(),
		toolFileInfo(),
		toolSearchFiles(),
		toolRunBackgroundCommand(),
		toolListProcesses(),
		toolKillProcess(),
	}
}

// --- Sandbox Management ---

func toolCreateSandbox() mcp.Tool {
	return mcp.NewTool(
		"create_sandbox",
		mcp.WithDescription("Create a new sandbox VM. Same request fields as POST /sandboxes (CreateSandboxRequest). Returns sandbox details including id."),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Unique name for the sandbox (DNS-1123 subdomain format: lowercase alphanumeric and hyphens)"),
		),
		mcp.WithString("image",
			mcp.Description("Image name in name or name:ver form (e.g. code, docker-lite, max, docker). Defaults to code if omitted."),
		),
		mcp.WithNumber("cpu",
			mcp.Description("Number of vCPUs (1-8). Defaults to 1."),
		),
		mcp.WithNumber("mem",
			mcp.Description("Memory in MiB (1024-16384). Defaults to 1024."),
		),
		mcp.WithString("orgId",
			mcp.Description("Same as REST CreateSandboxRequest; always set from the API key on create (body value is not used)."),
		),
		mcp.WithString("userId",
			mcp.Description("Same as REST CreateSandboxRequest; always set from the API key on create (body value is not used)."),
		),
		mcp.WithBoolean("sync",
			mcp.Description("If true (default), block until the guest agent is ready (~2s check)."),
		),
		mcp.WithObject("envVars",
			mcp.Description("Environment variables for the sandbox (string map)."),
		),
		mcp.WithBoolean("autoSleep",
			mcp.Description("If true, auto-snapshot the VM after idle time."),
		),
		mcp.WithString("region",
			mcp.Description("Target region when supported by your account."),
		),
		mcp.WithArray("publishPorts",
			mcp.Description("Sandbox TCP ports to expose via the public gateway (voidrun-ee). Up to 4 ports (1-65535), no duplicates. Omit for no public ports."),
			mcp.Items(map[string]any{"type": "integer"}),
			mcp.MaxItems(4),
		),
	)
}

func toolListSandboxes() mcp.Tool {
	return mcp.NewTool(
		"list_sandboxes",
		mcp.WithDescription("List all sandboxes in the organization. Returns paginated results."),
		mcp.WithNumber("page",
			mcp.Description("Page number (1-based). Defaults to 1."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Number of results per page. Defaults to server config."),
		),
	)
}

func toolGetSandbox() mcp.Tool {
	return mcp.NewTool(
		"get_sandbox",
		mcp.WithDescription("Get details of a specific sandbox by ID. Returns name, status, resources, etc."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The sandbox ID"),
		),
	)
}

func toolDeleteSandbox() mcp.Tool {
	return mcp.NewTool(
		"delete_sandbox",
		mcp.WithDescription("Delete a sandbox and all its resources permanently."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The sandbox ID to delete"),
		),
	)
}

// --- Command Execution ---

func toolExecuteCommand() mcp.Tool {
	return mcp.NewTool(
		"execute_command",
		mcp.WithDescription("Execute a shell command in a sandbox and return the output. The sandbox must be running (it will be auto-restored if snapshotted)."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The sandbox ID"),
		),
		mcp.WithString("command",
			mcp.Required(),
			mcp.Description("Shell command to execute"),
		),
		mcp.WithNumber("timeout",
			mcp.Description("Timeout in seconds (1-300). Defaults to 30."),
		),
		mcp.WithString("cwd",
			mcp.Description("Working directory for the command. Defaults to /root."),
		),
	)
}

// --- Filesystem ---

func toolReadFile() mcp.Tool {
	return mcp.NewTool(
		"read_file",
		mcp.WithDescription("Read the contents of a file from a sandbox. Returns the file content as text."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The sandbox ID"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("Absolute path to the file inside the sandbox"),
		),
	)
}

func toolWriteFile() mcp.Tool {
	return mcp.NewTool(
		"write_file",
		mcp.WithDescription("Write content to a file in a sandbox. Creates the file if it doesn't exist, overwrites if it does."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The sandbox ID"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("Absolute path for the file inside the sandbox"),
		),
		mcp.WithString("content",
			mcp.Required(),
			mcp.Description("The text content to write to the file"),
		),
	)
}

func toolListFiles() mcp.Tool {
	return mcp.NewTool(
		"list_files",
		mcp.WithDescription("List files and directories at a given path in a sandbox."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The sandbox ID"),
		),
		mcp.WithString("path",
			mcp.Description("Directory path to list. Defaults to /root."),
		),
	)
}

func toolCreateDirectory() mcp.Tool {
	return mcp.NewTool(
		"create_directory",
		mcp.WithDescription("Create a directory (including parent directories) in a sandbox."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The sandbox ID"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("Absolute path of the directory to create"),
		),
	)
}

func toolDeleteFile() mcp.Tool {
	return mcp.NewTool(
		"delete_file",
		mcp.WithDescription("Delete a file or directory from a sandbox."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The sandbox ID"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("Absolute path of the file or directory to delete"),
		),
	)
}

func toolMoveFile() mcp.Tool {
	return mcp.NewTool(
		"move_file",
		mcp.WithDescription("Move or rename a file or directory within a sandbox."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The sandbox ID"),
		),
		mcp.WithString("from",
			mcp.Required(),
			mcp.Description("Source path"),
		),
		mcp.WithString("to",
			mcp.Required(),
			mcp.Description("Destination path"),
		),
	)
}

func toolFileInfo() mcp.Tool {
	return mcp.NewTool(
		"file_info",
		mcp.WithDescription("Get file or directory metadata (size, permissions, modification time, etc.) in a sandbox."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The sandbox ID"),
		),
		mcp.WithString("path",
			mcp.Required(),
			mcp.Description("Absolute path to the file or directory"),
		),
	)
}

func toolSearchFiles() mcp.Tool {
	return mcp.NewTool(
		"search_files",
		mcp.WithDescription("Search for files matching a pattern within a directory in a sandbox."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The sandbox ID"),
		),
		mcp.WithString("path",
			mcp.Description("Directory to search in. Defaults to /root."),
		),
		mcp.WithString("pattern",
			mcp.Required(),
			mcp.Description("Search pattern (glob or filename substring)"),
		),
	)
}

// --- Background Processes ---

func toolRunBackgroundCommand() mcp.Tool {
	return mcp.NewTool(
		"run_background_command",
		mcp.WithDescription("Start a long-running background process in a sandbox. Returns a PID that can be used with list_processes and kill_process."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The sandbox ID"),
		),
		mcp.WithString("command",
			mcp.Required(),
			mcp.Description("Shell command to run in the background"),
		),
		mcp.WithString("cwd",
			mcp.Description("Working directory. Defaults to /root."),
		),
	)
}

func toolListProcesses() mcp.Tool {
	return mcp.NewTool(
		"list_processes",
		mcp.WithDescription("List all running background processes in a sandbox."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The sandbox ID"),
		),
	)
}

func toolKillProcess() mcp.Tool {
	return mcp.NewTool(
		"kill_process",
		mcp.WithDescription("Kill a background process by PID in a sandbox."),
		mcp.WithString("id",
			mcp.Required(),
			mcp.Description("The sandbox ID"),
		),
		mcp.WithNumber("pid",
			mcp.Required(),
			mcp.Description("Process ID to kill"),
		),
	)
}
