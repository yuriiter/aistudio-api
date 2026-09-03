package services

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mxschmitt/playwright-go"
)

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
	mu        sync.Mutex

	runMu   sync.Mutex
	runPage playwright.Page
}

// captureState holds the GenerateContent response bodies seen since the last
// run started. Runs are serialized, so a single shared bucket is safe.
type captureState struct {
	mu     sync.Mutex
	bodies [][]byte
	done   chan bool
}

var capture captureState

var Manager *PlaywrightManager

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

func InitAndConnect() (*PlaywrightManager, error) {
	if Manager != nil {
		log.Println("Cleaning up previous manager instance before re-initialization...")
		Manager.Cleanup()
	}

	for {
		fmt.Println("\n--- Initialization Step 1: Starting Headless Verification ---")

		pm := &PlaywrightManager{}
		var err error

		pm.pw, err = playwright.Run()
		if err != nil {
			return nil, fmt.Errorf("failed to start Playwright: %w", err)
		}

		pm.ChromeCmd, err = pm.StartChromium(true)
		if err != nil {
			pm.pw.Stop()
			return nil, err
		}

		pm.Browser, err = pm.connectOverCDP()
		if err != nil {
			pm.Cleanup()
			return nil, fmt.Errorf("failed to connect: %w", err)
		}

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
		applyStealthInitScripts(pm.Context)
		applyResponseCaptureHook(pm.Context)

		fmt.Println("Checking login status...")
		loggedIn, err := pm.checkIfLoggedIn()
		if err != nil {
			pm.Cleanup()
			return nil, fmt.Errorf("check login error: %w", err)
		}

		if loggedIn {
			fmt.Println(">> Status: LOGGED IN. Starting Server...")
			Manager = pm
			return pm, nil
		}

		fmt.Println(">> Status: NOT LOGGED IN.")
		fmt.Println(">> Closing headless browser and opening visible window for manual login.")

		pm.Cleanup()
		time.Sleep(2 * time.Second)

		err = runHeadfulLoginSession()
		if err != nil {
			return nil, err
		}

		fmt.Println(">> Browser closed. Restarting background verification...")
		time.Sleep(1 * time.Second)
	}
}

func (pm *PlaywrightManager) checkIfLoggedIn() (bool, error) {
	page, err := pm.CreateNewPage()
	if err != nil {
		return false, err
	}
	defer page.Close()

	_, err = page.Goto(NEW_PROMPT_PAGE, playwright.PageGotoOptions{
		Timeout: playwright.Float(15000),
	})

	if err != nil {
	}

	time.Sleep(2 * time.Second)

	url := page.URL()
	if strings.Contains(url, "accounts.google.com") {
		return false, nil
	}
	if strings.Contains(url, "aistudio.google.com") {
		return true, nil
	}

	return false, fmt.Errorf("unknown page loaded: %s", url)
}

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
		fmt.Sprintf("--user-data-dir=%s", userDataDir),
		"--window-position=10000,10000",
		fmt.Sprintf("--profile-directory=%s", CHROME_PROFILE_NAME),
		NEW_PROMPT_PAGE,
	}

	cmd := exec.Command(chromiumExec, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to launch headful chrome: %w", err)
	}

	if err := cmd.Wait(); err != nil {
	}

	return nil
}

func (pm *PlaywrightManager) StartChromium(headless bool) (*exec.Cmd, error) {
	userDataDir := getChromeUserDataDir()
	chromiumExec := getChromiumExecutable()

	args := []string{
		fmt.Sprintf("--remote-debugging-port=%s", DEBUG_PORT),
		fmt.Sprintf("--user-data-dir=%s", userDataDir),
		fmt.Sprintf("--profile-directory=%s", CHROME_PROFILE_NAME),
	}

	cmd := exec.Command(chromiumExec, args...)
	if headless {
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}

	err := cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("failed to start Chromium: %w", err)
	}

	fmt.Printf("Chromium started (Headless: %v) PID: %d\n", headless, cmd.Process.Pid)
	time.Sleep(2 * time.Second)

	return cmd, nil
}

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

func applyStealthInitScripts(ctx playwright.BrowserContext) {
	webdriverSpoof := "Object.defineProperty(navigator, 'webdriver', { get: () => undefined });"
	if err := ctx.AddInitScript(playwright.Script{
		Content: playwright.String(webdriverSpoof),
	}); err != nil {
		log.Printf("!! Failed to add init script: %v\n", err)
	}
}

func applyResponseCaptureHook(ctx playwright.BrowserContext) {
	ctx.OnResponse(func(response playwright.Response) {
		if !strings.Contains(response.URL(), "GenerateContent") {
			return
		}
		log.Println(">> [Net] Detected 'GenerateContent' stream...")

		go func() {
			body, err := response.Body()
			if err != nil {
				log.Printf("!! [Net] Error reading body: %v\n", err)
				return
			}

			log.Printf(">> [Net] Stream closed. Captured %d bytes.\n", len(body))

			if strings.Contains(string(body), "permission") || strings.Contains(string(body), "denied") {
				log.Printf("!! [Net] Possible abuse/permission block in body: %s\n", truncate(string(body), 300))
			}

			capture.mu.Lock()
			capture.bodies = append(capture.bodies, body)
			capture.mu.Unlock()

			select {
			case capture.done <- true:
			default:
			}
		}()
	})
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// AcquireRunPage returns the single long-lived AI Studio page, creating it if
// needed. Runs are serialized on this page so repeated requests look like one
// human working in one window instead of a fresh tab per request.
func (pm *PlaywrightManager) AcquireRunPage() (playwright.Page, error) {
	pm.runMu.Lock()

	if pm.Context == nil || pm.Browser == nil || !pm.Browser.IsConnected() {
		log.Println("Browser disconnected, attempting restart...")
		pm.Cleanup()
		newPm, err := InitAndConnect()
		if err != nil {
			pm.runMu.Unlock()
			return nil, err
		}
		pm.pw = newPm.pw
		pm.ChromeCmd = newPm.ChromeCmd
		pm.Browser = newPm.Browser
		pm.Context = newPm.Context
		pm.runPage = newPm.runPage
	}

	if pm.runPage == nil || pm.runPage.IsClosed() {
		page, err := pm.Context.NewPage()
		if err != nil {
			pm.runMu.Unlock()
			return nil, err
		}
		pm.runPage = page
	}

	return pm.runPage, nil
}

func (pm *PlaywrightManager) ReleaseRunPage() {
	pm.runMu.Unlock()
}

func (pm *PlaywrightManager) CreateNewPage() (playwright.Page, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.Browser == nil || !pm.Browser.IsConnected() {
		log.Println("Browser disconnected, attempting restart...")

		pm.Cleanup()

		newPm, err := InitAndConnect()
		if err != nil {
			return nil, err
		}

		pm.pw = newPm.pw
		pm.ChromeCmd = newPm.ChromeCmd
		pm.Browser = newPm.Browser
		pm.Context = newPm.Context
		applyStealthInitScripts(pm.Context)
	} else {
		applyStealthInitScripts(pm.Context)
	}

	if pm.Context == nil {
		return nil, fmt.Errorf("playwright context is not initialized")
	}
	return pm.Context.NewPage()
}
