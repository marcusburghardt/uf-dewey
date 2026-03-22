package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/unbound-force/dewey/backend"
	"github.com/unbound-force/dewey/client"
	"github.com/unbound-force/dewey/vault"
)

var version = "dev"

func main() {
	// Handle top-level help and version flags.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "-h", "-help", "--help", "help":
			printUsage()
			return
		case "-v", "-version", "--version":
			fmt.Println(version)
			return
		}
	}

	// Route to subcommand if first arg is a known command (not a flag).
	if len(os.Args) >= 2 && !strings.HasPrefix(os.Args[1], "-") {
		c := client.New("", "")
		switch os.Args[1] {
		case "serve":
			runServe(os.Args[2:])
		case "journal":
			runJournal(os.Args[2:], c)
		case "add":
			runAdd(os.Args[2:], c)
		case "search":
			runSearch(os.Args[2:], c)
		case "version":
			fmt.Println(version)
		default:
			fmt.Fprintf(os.Stderr, "dewey: unknown command %q\n\n", os.Args[1])
			printUsage()
			os.Exit(1)
		}
		return
	}

	// Default: MCP server (backward compatible).
	runServe(os.Args[1:])
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	readOnly := fs.Bool("read-only", false, "Disable all write operations")
	backendType := fs.String("backend", "", "Backend type: logseq (default) or obsidian")
	vaultPath := fs.String("vault", "", "Path to Obsidian vault (required for obsidian backend)")
	dailyFolder := fs.String("daily-folder", "daily notes", "Daily notes subfolder name (obsidian only)")
	httpAddr := fs.String("http", "", "HTTP address to listen on (e.g. :8080). Uses streamable HTTP transport instead of stdio.")
	fs.Parse(args)

	// Resolve backend from flag or environment.
	bt := *backendType
	if bt == "" {
		bt = os.Getenv("DEWEY_BACKEND")
	}
	if bt == "" {
		bt = "logseq"
	}

	var b backend.Backend
	switch bt {
	case "obsidian":
		vp := *vaultPath
		if vp == "" {
			vp = os.Getenv("OBSIDIAN_VAULT_PATH")
		}
		if vp == "" {
			fmt.Fprintf(os.Stderr, "dewey: --vault or OBSIDIAN_VAULT_PATH required for obsidian backend\n")
			os.Exit(1)
		}
		vc := vault.New(vp, vault.WithDailyFolder(*dailyFolder))
		if err := vc.Load(); err != nil {
			fmt.Fprintf(os.Stderr, "dewey: failed to load vault: %v\n", err)
			os.Exit(1)
		}
		vc.BuildBacklinks()
		
		// Start file watcher.
		if err := vc.Watch(); err != nil {
			fmt.Fprintf(os.Stderr, "dewey: failed to start watcher: %v\n", err)
			os.Exit(1)
		}
		defer vc.Close()
		
		b = vc
	case "logseq":
		lsClient := client.New("", "")
		checkGraphVersionControl(lsClient)
		b = lsClient
	default:
		fmt.Fprintf(os.Stderr, "dewey: unknown backend %q (use logseq or obsidian)\n", bt)
		os.Exit(1)
	}

	srv := newServer(b, *readOnly)

	if *httpAddr != "" {
		// Streamable HTTP transport — serves multiple clients.
		handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
			return srv
		}, nil)
		fmt.Fprintf(os.Stderr, "dewey: listening on %s\n", *httpAddr)
		if err := http.ListenAndServe(*httpAddr, handler); err != nil {
			fmt.Fprintf(os.Stderr, "dewey: %v\n", err)
			os.Exit(1)
		}
	} else {
		// Default: stdio transport for MCP client integration.
		if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			fmt.Fprintf(os.Stderr, "dewey: %v\n", err)
			os.Exit(1)
		}
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "dewey %s — Knowledge graph MCP server & CLI\n\n", version)
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  dewey                     Start MCP server (default, Logseq)\n")
	fmt.Fprintf(os.Stderr, "  dewey serve [flags]        Start MCP server\n")
	fmt.Fprintf(os.Stderr, "  dewey journal [flags] TEXT Append block to today's journal\n")
	fmt.Fprintf(os.Stderr, "  dewey add -p PAGE TEXT     Append block to a page\n")
	fmt.Fprintf(os.Stderr, "  dewey search QUERY         Full-text search across the graph\n")
	fmt.Fprintf(os.Stderr, "  dewey version              Print version\n")
	fmt.Fprintf(os.Stderr, "\nServe flags:\n")
	fmt.Fprintf(os.Stderr, "  --backend logseq|obsidian       Backend type (default: logseq)\n")
	fmt.Fprintf(os.Stderr, "  --vault PATH                    Obsidian vault path\n")
	fmt.Fprintf(os.Stderr, "  --daily-folder NAME             Daily notes folder (default: daily notes)\n")
	fmt.Fprintf(os.Stderr, "  --read-only                     Disable write operations\n")
	fmt.Fprintf(os.Stderr, "  --http ADDR                     Listen on HTTP (e.g. :8080) instead of stdio\n")
	fmt.Fprintf(os.Stderr, "\nAll CLI commands read from stdin when no TEXT argument is given.\n")
	fmt.Fprintf(os.Stderr, "Environment: LOGSEQ_API_URL (default http://127.0.0.1:12315)\n")
	fmt.Fprintf(os.Stderr, "             LOGSEQ_API_TOKEN\n")
	fmt.Fprintf(os.Stderr, "             DEWEY_BACKEND   Backend type\n")
	fmt.Fprintf(os.Stderr, "             OBSIDIAN_VAULT_PATH   Obsidian vault path\n")
}

// checkGraphVersionControl warns on stderr if the Logseq graph is not git-controlled.
// Best-effort: silently skips if Logseq is not running or the API is unreachable.
func checkGraphVersionControl(c *client.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	graph, err := c.GetCurrentGraph(ctx)
	if err != nil || graph == nil || graph.Path == "" {
		return
	}

	gitDir := filepath.Join(graph.Path, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "dewey: WARNING: graph %q at %s is not version controlled\n", graph.Name, graph.Path)
		fmt.Fprintf(os.Stderr, "dewey: Write operations cannot be undone. Consider: cd %s && git init && git add -A && git commit -m 'initial'\n", graph.Path)
	}
}
