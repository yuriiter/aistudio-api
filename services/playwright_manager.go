package services

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"
)

// Constants
const (
	NEW_PROMPT_PAGE     = "https://aistudio.google.com/prompts/new_chat"
	GOOGLE_SIGNIN_PAGE  = "https://accounts.google.com"
	CHROME_PROFILE_NAME = "Profile 1"
	DEBUG_PORT          = "9222"
)

type PlaywrightManager struct {
	pw        *playwright.Playwright
	ChromeCmd *exec.Cmd
	Browser   playwright.Browser
	Context   playwright.BrowserContext
}

var Manager *PlaywrightManager

// --- Utility Functions ---

func getChromeUserDataDir() string {
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

// --- Logic ---

// InitAndConnect implements the logic: Headless Check -> Fail? -> Headful Login -> Loop
func InitAndConnect() (*PlaywrightManager, error) {
	for {
		fmt.Println("\n--- Initialization Step 1: Starting Headless Verification ---")

		// 1. Start a temporary manager for headless check
		pm := &PlaywrightManager{}
		var err error

		pm.pw, err = playwright.Run()
		if err != nil {
			return nil, fmt.Errorf("failed to start Playwright: %w", err)
		}

		// Start Headless
		pm.ChromeCmd, err = pm.StartChromium(true) // true = HEADLESS
		if err != nil {
			pm.pw.Stop()
			return nil, err
		}

		// Connect
		pm.Browser, err = pm.connectOverCDP()
		if err != nil {
			pm.Cleanup()
			return nil, fmt.Errorf("failed to connect: %w", err)
		}

		// Create Context
		contexts := pm.Browser.Contexts()
		if len(contexts) == 0 {
			pm.Context, err = pm.Browser.NewContext()
		} else {
			pm.Context = contexts[0]
		}
		if err != nil {
			pm.Cleanup()
			return nil, err
		}

		// Check Login Status
		fmt.Println("Checking login status...")
		loggedIn, err := pm.checkIfLoggedIn()
		if err != nil {
			pm.Cleanup()
			return nil, fmt.Errorf("check login error: %w", err)
		}

		if loggedIn {
			fmt.Println(">> Status: LOGGED IN. Starting Server...")
			Manager = pm // Set global
			return pm, nil
		}

		// 2. Not Logged In Logic
		fmt.Println(">> Status: NOT LOGGED IN.")
		fmt.Println(">> Closing headless browser and opening visible window for manual login.")

		// Close the headless instance completely
		pm.Cleanup()
		time.Sleep(2 * time.Second) // Wait for port 9222 to free up

		// 3. Open Headful Browser
		// We don't need Playwright here, just the process to wait for.
		err = runHeadfulLoginSession()
		if err != nil {
			return nil, err
		}

		// 4. User closed the window, loop back to Step 1 to verify
		fmt.Println(">> Browser closed. Restarting background verification...")
		time.Sleep(1 * time.Second)
	}
}

// checkIfLoggedIn navigates to AI Studio and returns true if successful, false if redirected to login
func (pm *PlaywrightManager) checkIfLoggedIn() (bool, error) {
	page, err := pm.CreateNewPage()
	if err != nil {
		return false, err
	}
	defer page.Close()

	// Short timeout for the check
	_, err = page.Goto(NEW_PROMPT_PAGE, playwright.PageGotoOptions{
		Timeout: playwright.Float(15000),
	})

	// If navigation fails entirely (e.g. no internet), return error
	if err != nil {
		// Sometimes goto fails on redirects, check URL anyway
	}

	time.Sleep(2 * time.Second) // Let redirects settle

	url := page.URL()
	if strings.Contains(url, "accounts.google.com") {
		return false, nil
	}
	if strings.Contains(url, "aistudio.google.com") {
		return true, nil
	}

	return false, fmt.Errorf("unknown page loaded: %s", url)
}

// runHeadfulLoginSession starts Chrome visibly and BLOCKS until the user closes it
func runHeadfulLoginSession() error {
	userDataDir := getChromeUserDataDir()
	chromiumExec := getChromiumExecutable()

	fmt.Println("\n***************************************************")
	fmt.Println("* ACTION REQUIRED                                 *")
	fmt.Println("* 1. Browser has opened in VISIBLE mode.          *")
	fmt.Println("* 2. Please log in to Google AI Studio.           *")
	fmt.Println("* 3. WHEN DONE, CLOSE THE BROWSER WINDOW.         *")
	fmt.Println("***************************************************")

	args := []string{
		// No remote-debugging-port needed here strictly, but good for consistency
		// No headless flag
		fmt.Sprintf("--user-data-dir=%s", userDataDir),
		fmt.Sprintf("--profile-directory=%s", CHROME_PROFILE_NAME),
		NEW_PROMPT_PAGE, // Open directly to the page
	}

	cmd := exec.Command(chromiumExec, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to launch headful chrome: %w", err)
	}

	// This blocks until the user closes the window
	if err := cmd.Wait(); err != nil {
		// Ignore exit status errors (common when closing windows)
	}

	return nil
}

// StartChromium launches the browser process
func (pm *PlaywrightManager) StartChromium(headless bool) (*exec.Cmd, error) {
	userDataDir := getChromeUserDataDir()
	chromiumExec := getChromiumExecutable()

	args := []string{
		fmt.Sprintf("--remote-debugging-port=%s", DEBUG_PORT),
		fmt.Sprintf("--user-data-dir=%s", userDataDir),
		fmt.Sprintf("--profile-directory=%s", CHROME_PROFILE_NAME),
	}

	// if headless {
	// 	args = append(args, "--headless=new")
	// }

	cmd := exec.Command(chromiumExec, args...)
	// Suppress output in headless to keep logs clean, show in headful
	if headless {
		// cmd.Stdout = nil
		// cmd.Stderr = nil
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}

	err := cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("failed to start Chromium: %w", err)
	}

	fmt.Printf("Chromium started (Headless: %v) PID: %d\n", headless, cmd.Process.Pid)
	time.Sleep(2 * time.Second) // Give it a moment to start

	return cmd, nil
}

// ConnectOverCDP connects Playwright to the running browser
func (pm *PlaywrightManager) connectOverCDP() (playwright.Browser, error) {
	cdpURL := fmt.Sprintf("http://localhost:%s", DEBUG_PORT)

	for i := 0; i < 5; i++ {
		browser, err := pm.pw.Chromium.ConnectOverCDP(cdpURL)
		if err == nil {
			return browser, nil
		}
		time.Sleep(1 * time.Second)
	}
	return nil, fmt.Errorf("failed to connect to Chrome CDP")
}

// Cleanup gracefully shuts down the browser and Playwright.
func (pm *PlaywrightManager) Cleanup() {
	if pm.Browser != nil {
		pm.Browser.Close()
	}
	if pm.ChromeCmd != nil && pm.ChromeCmd.Process != nil {
		pm.ChromeCmd.Process.Kill()
	}
	if pm.pw != nil {
		pm.pw.Stop()
	}
}

// CreateNewPage is a utility for API handlers to get a fresh page
func (pm *PlaywrightManager) CreateNewPage() (playwright.Page, error) {
	if pm.Context == nil {
		return nil, fmt.Errorf("playwright context is not initialized")
	}
	return pm.Context.NewPage()
}
