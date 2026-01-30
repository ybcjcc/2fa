package main

import (
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pquerna/otp/totp"
	"google.golang.org/protobuf/proto"
)

type Account struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
	Issuer string `json:"issuer,omitempty"`
}

func main() {
	flag.Usage = printHelp
	// Define flags but don't parse globally until we know if it's a subcommand
	// Actually, standard flag package is quirky with subcommands.
	// Let's use FlagSets for better subcommand handling.

	addCmd := flag.NewFlagSet("add", flag.ExitOnError)
	addName := addCmd.String("name", "", "Account Name")
	addSecret := addCmd.String("secret", "", "Secret Key")

	importCmd := flag.NewFlagSet("import", flag.ExitOnError)
	importFile := importCmd.String("file", "", "File path")

	// Global search flag (hacky: if no subcommand, parse global)
	// Simple approach: Check os.Args[1]

	args := os.Args
	if len(args) < 2 {
		// List mode default
		entries, _ := loadAccounts()
		printCodes(entries, "")
		return
	}

	cmd := args[1]

	entries, err := loadAccounts()
	if err != nil {
		entries = []Account{}
	}

	switch cmd {
	case "add":
		addCmd.Parse(args[2:])
		if *addName == "" || *addSecret == "" {
			fmt.Println("Error: name and secret are required")
			return
		}
		// Validate Secret
		secret := strings.ToUpper(strings.ReplaceAll(*addSecret, " ", ""))
		// Try decode to validate
		if _, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret); err != nil {
			if _, err := base32.StdEncoding.DecodeString(secret); err != nil {
				fmt.Println("Error: Invalid Base32 secret")
				return
			}
		}
		entries = append(entries, Account{Name: *addName, Secret: secret})
		saveAccounts(entries)
		fmt.Println("Account added.")

	case "import":
		// Handle direct file arg like: 2fa import export.txt
		if len(args) > 2 && !strings.HasPrefix(args[2], "-") {
			// Assume args[2] is the filename
			*importFile = args[2]
		} else {
			importCmd.Parse(args[2:])
		}

		if *importFile == "" {
			fmt.Println("Error: file path required. Usage: 2fa import <file>")
			return
		}

		content, err := ioutil.ReadFile(*importFile)
		if err != nil {
			fmt.Printf("Error reading file: %v\n", err)
			return
		}
		strContent := string(content)
		lines := strings.Split(strContent, "\n")
		count := 0
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			u, err := url.Parse(line)
			if err != nil {
				continue
			}

			if u.Scheme == "otpauth-migration" {
				data := u.Query().Get("data")
				if data != "" {
					decodedData, err := base64.StdEncoding.DecodeString(data)
					if err == nil {
						var payload MigrationPayload
						if err := proto.Unmarshal(decodedData, &payload); err == nil {
							for _, p := range payload.OtpParameters {
								secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(p.Secret)
								name := p.Name
								if p.Issuer != "" {
									name = p.Issuer + ":" + name
								}

								// Check duplicate (by Secret OR Name)
								exists := false
								for _, e := range entries {
									// If secret matches, it's definitely same account.
									// If name matches, we treat as duplicate to avoid confusion, or should we update?
									// Usually secret match is the strong check.
									if e.Secret == secret {
										exists = true
										break
									}
									// Optional: check strictly by name too?
									// if e.Name == name { exists = true; break }
								}
								if !exists {
									entries = append(entries, Account{Name: name, Secret: secret, Issuer: p.Issuer})
									count++
								}
							}
						}
					}
				}
			}
		}
		saveAccounts(entries)
		fmt.Printf("Imported %d new accounts.\n", count)

	case "delete", "rm", "del":
		if len(args) < 3 {
			fmt.Println("Error: name required. Usage: 2fa delete <name>")
			return
		}
		target := args[2]
		newEntries := []Account{}
		deleted := 0
		for _, acc := range entries {
			if strings.Contains(strings.ToLower(acc.Name), strings.ToLower(target)) || strings.Contains(strings.ToLower(acc.Issuer), strings.ToLower(target)) {
				fmt.Printf("Deleted: %s\n", acc.Name)
				deleted++
				continue
			}
			newEntries = append(newEntries, acc)
		}
		if deleted > 0 {
			saveAccounts(newEntries)
		} else {
			fmt.Println("No accounts matched.")
		}

	case "help", "-h", "--help":
		printHelp()

	default:
		// Default to list with search filter if arg provided not a command
		if strings.HasPrefix(cmd, "-") {
			// It's a flag?
			// Just treat entire args[1] as filter if it doesn't look like flag
			printCodes(entries, "")
		} else {
			printCodes(entries, cmd)
		}
	}
}

func printCodes(entries []Account, filter string) {
	// Dynamically calculate max width for name column
	maxNameLen := 10
	for _, acc := range entries {
		l := len(acc.Name)
		// Consider wide characters (Chinese) calculation?
		// For simple align, standard len is usually okay-ish unless mixed.
		// Let's cap it reasonably so it fits on standard terminals.
		if l > maxNameLen {
			maxNameLen = l
		}
	}
	if maxNameLen > 40 {
		maxNameLen = 40
	}

	// Format string
	fmtStr := fmt.Sprintf("%%-%ds | %%-10s | %%s\n", maxNameLen)

	fmt.Printf(fmtStr, "Account", "Code", "Expires")
	fmt.Println(strings.Repeat("-", maxNameLen+20))

	now := time.Now()
	// Calculate remaining seconds for 30s window
	period := 30
	unix := now.Unix()
	remainder := period - int(unix%int64(period))

	// ansi color for progress
	color := "\033[32m" // Green
	if remainder < 10 {
		color = "\033[31m" // Red
	} else if remainder < 20 {
		color = "\033[33m" // Yellow
	}

	for _, acc := range entries {
		if filter != "" && !strings.Contains(strings.ToLower(acc.Name), strings.ToLower(filter)) && !strings.Contains(strings.ToLower(acc.Issuer), strings.ToLower(filter)) {
			continue
		}

		code, err := totp.GenerateCode(acc.Secret, now)
		if err != nil {
			code = "ERROR"
		}

		name := acc.Name
		// Truncate logic
		// If name is longer than maxNameLen, truncate with ..
		// But wait, we set maxNameLen based on longest.
		// If longest > 40, we cap at 40.
		if len(name) > maxNameLen {
			// Careful with slicing multibyte strings
			runes := []rune(name)
			if len(runes) > maxNameLen-2 {
				name = string(runes[:maxNameLen-2]) + ".."
			}
		}

		fmt.Printf(fmtMap(fmtStr, name, color, code, remainder))
	}
	fmt.Println(strings.Repeat("-", maxNameLen+20))
}

func fmtMap(format, name, color, code string, rem int) string {
	// Helper to inject color which might mess up Printf width calculation if included in string arg
	return fmt.Sprintf(format, name, color+code+"\033[0m", fmt.Sprintf("%d s", rem))
}

func getStorePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	configDir := filepath.Join(home, ".2fa")
	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		if err := os.MkdirAll(configDir, 0700); err != nil {
			return "", err
		}
	}
	return filepath.Join(configDir, "accounts.json"), nil
}

func loadAccounts() ([]Account, error) {
	path, err := getStorePath()
	if err != nil {
		return nil, err
	}

	// Migration logic: Check if new file exists, if not, check legacy
	if _, err := os.Stat(path); os.IsNotExist(err) {
		home, _ := os.UserHomeDir()
		legacyPath := filepath.Join(home, ".2fa.json")
		if _, err := os.Stat(legacyPath); err == nil {
			// Found legacy file, migrate
			fmt.Printf("Migrating legacy data from %s to %s...\n", legacyPath, path)
			input, err := ioutil.ReadFile(legacyPath)
			if err != nil {
				return nil, fmt.Errorf("failed to read legacy file: %v", err)
			}
			if err := ioutil.WriteFile(path, input, 0600); err != nil {
				return nil, fmt.Errorf("failed to write new config file: %v", err)
			}
			// Optional: Remove legacy file or rename it
			os.Rename(legacyPath, legacyPath+".bak")
		} else {
			// No new file, no legacy file -> Fresh start
			return []Account{}, nil
		}
	}

	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var accs []Account
	err = json.Unmarshal(data, &accs)
	return accs, err
}

func saveAccounts(accs []Account) error {
	path, err := getStorePath()
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(accs, "", "  ")
	return ioutil.WriteFile(path, data, 0600)
}

func printHelp() {
	help := `2fa - Command Line 2FA Authenticator

Usage: 2fa [command] [flags]

Commands:
  list (default)      Show all codes (supports fuzzy search argument)
  add                 Add a new account manually
  delete (rm, del)    Delete accounts (fuzzy name match)
  import              Import from Google Authenticator export text file

Flags:
  -name     Account Name (for add)
  -secret   Secret Key (Base32, spaces allowed) (for add)
  -import   Path to file containing otpauth-migration:// links

Examples:
  2fa                       # List all
  2fa git                   # Search for "git"
  2fa add -name "MyGoogle" -secret "JBSWY3DPEHPK3PXP"
  2fa del "test account"    # Delete account matching "test account"
  2fa import dump.txt       # Import from file

Importing from Google Authenticator:
  1. In App, select "Transfer accounts" -> "Export accounts".
  2. Use a QR scanner to get the "otpauth-migration://..." URL.
  3. If there are multiple QR codes, paste each URL on a new line in a text file.
  4. Run: 2fa import your_file.txt
`
	fmt.Println(help)
}
