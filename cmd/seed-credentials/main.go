// Run this once (and again any time you rotate the app-specific password)
// to store the Apple ID + app-specific password used to authenticate to
// iCloud's CalDAV server in Azure Key Vault.
//
// Usage:
//
//	ICLOUD_KEY_VAULT_NAME=... go run ./cmd/seed-credentials
//
// You must be signed in to Azure (e.g. via `az login`) with write access to
// that vault. This is the Apple-credential equivalent of the Concept2 MCP
// server's bootstrap_auth.py — except there's no browser OAuth flow here,
// since iCloud CalDAV authenticates with a static app-specific password
// (generate one at https://appleid.apple.com, under "App-Specific
// Passwords"), not an OAuth token.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/twallac10/ios-mcp/internal/credentials"
)

func main() {
	vaultName := os.Getenv("ICLOUD_KEY_VAULT_NAME")
	if vaultName == "" {
		fmt.Fprintln(os.Stderr, "ICLOUD_KEY_VAULT_NAME must be set")
		os.Exit(1)
	}

	reader := bufio.NewReader(os.Stdin)

	fmt.Print("Apple ID (email): ")
	appleID, err := readLine(reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading Apple ID: %v\n", err)
		os.Exit(1)
	}

	// Typed into the running prompt (never a command-line argument), so it
	// never lands in shell history, argv, or any log — unlike
	// `az keyvault secret set --value ...`. On a terminal it isn't echoed;
	// piped input (stdin not a terminal) is read as a plain line.
	fmt.Print("App-specific password (from appleid.apple.com): ")
	password, err := readSecret(reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading app-specific password: %v\n", err)
		os.Exit(1)
	}

	if appleID == "" || password == "" {
		fmt.Fprintln(os.Stderr, "both the Apple ID and the app-specific password are required")
		os.Exit(1)
	}

	store, err := credentials.NewStore(vaultName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connecting to Key Vault %q: %v\n", vaultName, err)
		os.Exit(1)
	}

	ctx := context.Background()
	if err := store.Save(ctx, &credentials.Credentials{AppleID: appleID, AppSpecificPassword: password}); err != nil {
		fmt.Fprintf(os.Stderr, "saving credentials to Key Vault: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Saved. The server will pick this up on its next credential load (or immediately, if it's never loaded credentials yet).")
}

// readSecret reads one line without echoing it when stdin is a terminal,
// falling back to a plain buffered line read otherwise.
func readSecret(reader *bufio.Reader) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Println()
		return strings.TrimSpace(string(b)), err
	}
	return readLine(reader)
}

// readLine reads one trimmed line, accepting a final line with no trailing
// newline (e.g. piped input from printf without "\n").
func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err == io.EOF && line != "" {
		err = nil
	}
	return strings.TrimSpace(line), err
}
