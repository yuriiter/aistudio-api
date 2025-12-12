package main

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

const (
	NEW_PROMPT_PAGE     = "https://aistudio.google.com/prompts/new_chat"
	GOOGLE_SIGNIN_PAGE  = "https://accounts.google.com"
	CHROME_PROFILE_NAME = "Profile 1"
	DEBUG_PORT          = "9222"
)

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

func startChromium() *exec.Cmd {
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
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Start()
	if err != nil {
		log.Fatalf("Failed to start Chromium: %v", err)
	}

	fmt.Printf("Chromium started with PID: %d\n", cmd.Process.Pid)
	fmt.Printf("Remote debugging on port %s\n", DEBUG_PORT)

	time.Sleep(2 * time.Second)

	return cmd
}

func connectToChromium(pw *playwright.Playwright) (playwright.Browser, error) {
	cdpURL := fmt.Sprintf("http://localhost:%s", DEBUG_PORT)

	for i := 0; i < 10; i++ {
		browser, err := pw.Chromium.ConnectOverCDP(cdpURL)
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

func isLoggedInToGoogle(page playwright.Page) bool {
	fmt.Println("Checking if logged in to Google...")

	_, err := page.Goto("https://accounts.google.com/ServiceLogin", playwright.PageGotoOptions{
		Timeout: playwright.Float(10000),
	})
	if err != nil {
		fmt.Printf("Navigation error: %v\n", err)
		return false
	}

	time.Sleep(2 * time.Second)

	url := page.URL()
	fmt.Printf("Current URL: %s\n", url)

	if len(url) > 0 && (contains(url, "myaccount") || contains(url, "accounts.google.com/signin/v2/challenge")) {
		return true
	}

	loginForm, _ := page.QuerySelector("input[type='email']")
	return loginForm == nil
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func handleLogin(browser playwright.Browser, context playwright.BrowserContext) {
	fmt.Println("\n=== LOGIN REQUIRED ===")
	fmt.Println("Opening Google Sign-In page...")

	loginPage, err := context.NewPage()
	if err != nil {
		log.Fatalf("Failed to create login page: %v", err)
	}

	_, err = loginPage.Goto(GOOGLE_SIGNIN_PAGE, playwright.PageGotoOptions{
		Timeout: playwright.Float(30000),
	})
	if err != nil {
		log.Fatalf("Failed to navigate to Google sign-in: %v", err)
	}

	fmt.Println("\nPlease log in to Google in the browser window.")
	fmt.Println("After logging in, press Enter to continue...")
	fmt.Scanln()

	loginPage.Close()
	fmt.Println("Login window closed.")
}

func main() {
	fmt.Println("Starting Chrome automation...")

	chromeCmd := startChromium()
	defer func() {
		if chromeCmd.Process != nil {
			chromeCmd.Process.Kill()
		}
	}()

	pw, err := playwright.Run()
	if err != nil {
		log.Fatalf("Failed to start Playwright: %v", err)
	}
	defer pw.Stop()

	fmt.Println("\nConnecting to Chromium...")
	browser, err := connectToChromium(pw)
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer browser.Close()

	fmt.Println("Connected successfully!")

	contexts := browser.Contexts()
	if len(contexts) == 0 {
		log.Fatal("No browser contexts found")
	}
	context := contexts[0]

	var page playwright.Page
	pages := context.Pages()
	if len(pages) > 0 {
		page = pages[0]
	} else {
		page, err = context.NewPage()
		if err != nil {
			log.Fatalf("Failed to create page: %v", err)
		}
	}

	if !isLoggedInToGoogle(page) {
		fmt.Println("Not logged in to Google.")
		handleLogin(browser, context)
	} else {
		fmt.Println("Already logged in to Google!")
	}

	fmt.Println("\n=== NAVIGATING TO AI STUDIO ===")
	_, err = page.Goto(NEW_PROMPT_PAGE, playwright.PageGotoOptions{
		Timeout: playwright.Float(0),
	})
	if err != nil {
		log.Fatalf("Failed to navigate to AI Studio: %v", err)
	}

	title, _ := page.Title()
	fmt.Printf("Page loaded: %s\n", title)

	fmt.Println("\nAutomation ready! Press Enter to exit...")
	fmt.Scanln()
}
