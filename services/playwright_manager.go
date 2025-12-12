package services

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/playwright-community/playwright-go"
)

// Constants remain the same
const (
	NEW_PROMPT_PAGE     = "https://aistudio.google.com/prompts/new_chat"
	GOOGLE_SIGNIN_PAGE  = "https://accounts.google.com"
	CHROME_PROFILE_NAME = "Profile 1"
	DEBUG_PORT          = "9222"
)

// PlaywrightManager holds the necessary resources
type PlaywrightManager struct {
	pw        *playwright.Playwright
	ChromeCmd *exec.Cmd
	Browser   playwright.Browser
	Context   playwright.BrowserContext
}

// Global variable to hold the manager instance
var Manager *PlaywrightManager

// --- Utility Functions (Keep as is) ---

func getChromeUserDataDir() string {
	// ... (Your existing implementation)
	var userDataDir string
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		userDataDir = filepath.Join(home, "Library", "Application Support", "Chromium")
	case "linux":
		home, _ := os.UserHomeDir()
		userDataDir = filepath.Join(home, ".config", "chromium")
	case "windows":
		appData := os.Getenv("LocalAppData")
		userDataDir = filepath.Join(appData, "Chromium", "User Data")
	default:
		log.Fatal("Unsupported operating system")
	}
	return userDataDir
}

func getChromiumExecutable() string {
	// ... (Your existing implementation)
	switch runtime.GOOS {
	case "darwin":
		return "/Applications/Chromium.app/Contents/MacOS/Chromium"
	case "linux":
		return "chromium"
	case "windows":
		return "chromium.exe"
	default:
		log.Fatal("Unsupported operating system")
		return ""
	}
}

// --- PlaywrightManager Methods ---

// StartChromium launches the browser process
func (pm *PlaywrightManager) StartChromium() (*exec.Cmd, error) {
	userDataDir := getChromeUserDataDir()
	chromiumExec := getChromiumExecutable()

	fmt.Printf("Starting Chromium...\n")
	fmt.Printf("User Data Dir: %s\n", userDataDir)
	fmt.Printf("Profile: %s\n", CHROME_PROFILE_NAME)

	args := []string{
		fmt.Sprintf("--remote-debugging-port=%s", DEBUG_PORT),
		fmt.Sprintf("--user-data-dir=%s", userDataDir),
		fmt.Sprintf("--profile-directory=%s", CHROME_PROFILE_NAME),
	}

	cmd := exec.Command(chromiumExec, args...)
	// Note: In a server environment, you might want to suppress Stdout/Stderr or
	// pipe them elsewhere to avoid cluttering the server logs.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("failed to start Chromium: %w", err)
	}

	fmt.Printf("Chromium started with PID: %d\n", cmd.Process.Pid)
	fmt.Printf("Remote debugging on port %s\n", DEBUG_PORT)

	time.Sleep(2 * time.Second) // Give it a moment to start

	return cmd, nil
}

// ConnectOverCDP connects Playwright to the running browser
func (pm *PlaywrightManager) connectOverCDP() (playwright.Browser, error) {
	cdpURL := fmt.Sprintf("http://localhost:%s", DEBUG_PORT)

	for i := 0; i < 10; i++ {
		browser, err := pm.pw.Chromium.ConnectOverCDP(cdpURL)
		if err == nil {
			return browser, nil
		}
		if i < 9 {
			fmt.Printf("Connection attempt %d failed, retrying...\n", i+1)
			time.Sleep(time.Second)
		}
	}
	return nil, fmt.Errorf("failed to connect after 10 attempts")
}

// InitAndConnect sets up Playwright, starts the browser, and connects.
// This should be called once before the Gin server starts.
func InitAndConnect() (*PlaywrightManager, error) {
	fmt.Println("Starting Chrome automation initialization...")

	pm := &PlaywrightManager{}

	var err error
	pm.pw, err = playwright.Run()
	if err != nil {
		return nil, fmt.Errorf("failed to start Playwright: %w", err)
	}

	pm.ChromeCmd, err = pm.StartChromium()
	if err != nil {
		pm.pw.Stop()
		return nil, err
	}

	fmt.Println("\nConnecting to Chromium...")
	pm.Browser, err = pm.connectOverCDP()
	if err != nil {
		pm.Cleanup() // Clean up everything if connection fails
		return nil, fmt.Errorf("failed to connect: %w", err)
	}

	fmt.Println("Connected successfully!")

	contexts := pm.Browser.Contexts()
	if len(contexts) == 0 {
		// Create a new context if none exists (typical for CDP connect)
		pm.Context, err = pm.Browser.NewContext()
		if err != nil {
			pm.Cleanup()
			return nil, fmt.Errorf("failed to create browser context: %w", err)
		}
	} else {
		pm.Context = contexts[0]
	}

	// === Non-Interactive Login Check/Setup ===
	// You must now assume the profile is already logged in,
	// or implement a non-interactive way to log in.

	// Example: Try to ensure at least one page is open and navigate to AI Studio
	pages := pm.Context.Pages()
	var page playwright.Page
	if len(pages) > 0 {
		page = pages[0]
	} else {
		page, err = pm.Context.NewPage()
		if err != nil {
			pm.Cleanup()
			return nil, fmt.Errorf("failed to create initial page: %w", err)
		}
	}

	// Perform an initial check/navigation. The old isLoggedInToGoogle
	// and handleLogin must be removed or heavily modified.
	fmt.Println("\n=== Ensuring AI Studio access ===")
	_, err = page.Goto(NEW_PROMPT_PAGE, playwright.PageGotoOptions{
		Timeout: playwright.Float(30000), // Longer timeout for initial page load
	})
	if err != nil {
		fmt.Printf("Warning: Failed to navigate to AI Studio on init: %v. This may require manual login in the Chromium window before starting the server.\n", err)
	} else {
		title, _ := page.Title()
		fmt.Printf("Initial page loaded: %s\n", title)
	}

	Manager = pm // Set the global manager
	return pm, nil
}

// Cleanup gracefully shuts down the browser and Playwright.
// This should be called once when the Gin server shuts down.
func (pm *PlaywrightManager) Cleanup() {
	fmt.Println("\nShutting down Playwright resources...")
	if pm.Browser != nil {
		pm.Browser.Close()
	}
	if pm.ChromeCmd != nil && pm.ChromeCmd.Process != nil {
		fmt.Printf("Killing Chromium process PID: %d\n", pm.ChromeCmd.Process.Pid)
		pm.ChromeCmd.Process.Kill() // Force kill the browser
	}
	if pm.pw != nil {
		pm.pw.Stop()
	}
	fmt.Println("Cleanup complete.")
}

// CreateNewPage is a utility for API handlers to get a fresh page
func (pm *PlaywrightManager) CreateNewPage() (playwright.Page, error) {
	if pm.Context == nil {
		return nil, fmt.Errorf("playwright context is not initialized")
	}
	return pm.Context.NewPage()
}
