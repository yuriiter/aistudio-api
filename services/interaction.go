package services

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"aistudio-api/openai"
	"github.com/playwright-community/playwright-go"
)

// Selectors
const (
	SELECTOR_TEXTAREA = "textarea"
	// Selectors for file upload
	SELECTOR_ADD_MEDIA_BTN = "ms-add-media-button button"
	SELECTOR_UPLOAD_TEXT   = "Upload files" // Exact text search as requested

	// Idle = Ready to click (Generation finished)
	SELECTOR_RUN_IDLE = "button[aria-label='Run'][type='submit']"
	// Busy = Generating (Wait)
	SELECTOR_RUN_BUSY = "button[aria-label='Run'][type='button']"
)

// Configuration
const (
	MAX_RETRIES = 3
	RETRY_DELAY = 2 * time.Second
)

// ResponseCapture holds the captured network response
type ResponseCapture struct {
	mu       sync.Mutex
	chunks   []string
	complete bool
}

// Advanced JavaScript spy that handles the response directly
const NETWORK_SPY_SCRIPT = `
(() => {
	const originalXHROpen = XMLHttpRequest.prototype.open;
	const originalXHRSend = XMLHttpRequest.prototype.send;
	
	window._AI_ACCUMULATOR = ""; 
	window._AI_CAPTURE_DONE = false;
	window._AI_CAPTURE_STARTED = false;

	XMLHttpRequest.prototype.open = function(method, url, ...args) {
		this._url = url;
		return originalXHROpen.call(this, method, url, ...args);
	};

	XMLHttpRequest.prototype.send = function(...args) {
		if (this._url && this._url.includes('GenerateContent')) {
			window._AI_CAPTURE_STARTED = true;
			window._AI_ACCUMULATOR = "";
			window._AI_CAPTURE_DONE = false;

			const xhr = this;
			let lastLength = 0;
			let captureTimeout = null;
			
			this.addEventListener('readystatechange', function() {
				if (this.readyState === 3) {
					try {
						const currentText = this.responseText || "";
						if (currentText.length > lastLength) {
							window._AI_ACCUMULATOR = currentText;
							lastLength = currentText.length;
							if (captureTimeout) clearTimeout(captureTimeout);
							captureTimeout = setTimeout(() => {
								if (window._AI_ACCUMULATOR.length > 0 && !window._AI_CAPTURE_DONE) {
									window._AI_CAPTURE_DONE = true;
								}
							}, 2000);
						}
					} catch (e) {}
				}
				if (this.readyState === 4) {
					if (captureTimeout) clearTimeout(captureTimeout);
					window._AI_ACCUMULATOR = this.responseText || "";
					window._AI_CAPTURE_DONE = true;
				}
			});
			
			this.addEventListener('load', function() {
				if (!window._AI_CAPTURE_DONE) {
					window._AI_ACCUMULATOR = this.responseText || "";
					window._AI_CAPTURE_DONE = true;
				}
			});
			
			this.addEventListener('error', function() { window._AI_CAPTURE_DONE = true; });
		}
		return originalXHRSend.apply(this, args);
	};

	const originalFetch = window.fetch;
	window.fetch = async (...args) => {
		const response = await originalFetch(...args);
		const url = args[0] instanceof Request ? args[0].url : args[0];
		if (url && url.includes("GenerateContent")) {
			window._AI_CAPTURE_STARTED = true;
			window._AI_ACCUMULATOR = ""; 
			window._AI_CAPTURE_DONE = false;
			const clone = response.clone();
			const text = await clone.text();
			window._AI_ACCUMULATOR = text;
			window._AI_CAPTURE_DONE = true;
		}
		return response;
	};
})();
`

// ExecuteChatInteraction is the public wrapper that handles retries
func ExecuteChatInteraction(req openai.ChatCompletionRequest) (string, error) {
	var lastErr error

	for i := 0; i < MAX_RETRIES; i++ {
		if i > 0 {
			fmt.Printf(">> [Retry] Attempt %d/%d starting in %v...\n", i+1, MAX_RETRIES, RETRY_DELAY)
			time.Sleep(RETRY_DELAY)
		}

		fmt.Printf(">> [Attempt %d] Starting interaction...\n", i+1)
		response, err := executeAttempt(req)
		if err == nil {
			return response, nil
		}

		fmt.Printf(">> [Attempt %d] Failed: %v\n", i+1, err)
		lastErr = err
	}

	return "", fmt.Errorf("failed after %d attempts. Last error: %v", MAX_RETRIES, lastErr)
}

// executeAttempt contains the actual Playwright logic
func executeAttempt(req openai.ChatCompletionRequest) (string, error) {
	// Create a new page (thread-safe from Manager)
	page, err := Manager.CreateNewPage()
	if err != nil {
		return "", err
	}
	defer page.Close()

	// Inject the spy script
	if err := page.AddInitScript(playwright.Script{Content: playwright.String(NETWORK_SPY_SCRIPT)}); err != nil {
		return "", fmt.Errorf("spy inject failed: %v", err)
	}

	// Navigate
	targetURL := NEW_PROMPT_PAGE
	if req.Model != "" {
		targetURL = fmt.Sprintf("%s?model=%s", NEW_PROMPT_PAGE, req.Model)
	}
	if _, err := page.Goto(targetURL); err != nil {
		return "", err
	}

	// ---------------------------------------------------------
	// File Upload Logic
	// ---------------------------------------------------------
	fmt.Println(">> Preparing file upload...")

	// 1. Create temporary .txt file
	fullPrompt := buildPromptFromMessages(req.Messages)
	tmpFile, err := os.CreateTemp("", "aistudio-prompt-*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %v", err)
	}
	if _, err := tmpFile.WriteString(fullPrompt); err != nil {
		return "", fmt.Errorf("failed to write to temp file: %v", err)
	}
	tmpFilePath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpFilePath)

	// 2. Locate the "Add Media" button
	addMediaBtn := page.Locator(SELECTOR_ADD_MEDIA_BTN).First()
	if err := addMediaBtn.WaitFor(); err != nil {
		return "", fmt.Errorf("add media button not found")
	}

	// 3. Open the Media Menu explicitly
	fmt.Println(">> Opening Media Menu...")
	if err := addMediaBtn.Click(); err != nil {
		return "", fmt.Errorf("failed to click add media button: %v", err)
	}

	// 4. Locate the "UploadFiles" button in the dropdown
	uploadBtn := page.GetByText(SELECTOR_UPLOAD_TEXT).First()

	// Wait for the dropdown to render and the text to be visible
	if err := uploadBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	}); err != nil {
		return "", fmt.Errorf("dropdown opened, but text '%s' was not found/visible", SELECTOR_UPLOAD_TEXT)
	}

	// Tiny sleep to ensure the element is interactive
	time.Sleep(300 * time.Millisecond)

	// 5. Trigger File Chooser
	fmt.Println(">> Triggering file chooser...")
	fileChooser, err := page.ExpectFileChooser(func() error {
		return uploadBtn.Click()
	})
	if err != nil {
		return "", fmt.Errorf("failed to catch file chooser event: %v", err)
	}

	// 6. Set the file
	if err := fileChooser.SetFiles([]string{tmpFilePath}); err != nil {
		return "", fmt.Errorf("failed to set input files: %v", err)
	}
	fmt.Println(">> File attached.")

	// ---------------------------------------------------------
	// Execution
	// ---------------------------------------------------------

	// Click Run (IDLE state)
	fmt.Println(">> Clicking Run...")
	runBtn := page.Locator(SELECTOR_RUN_IDLE).First()
	if err := runBtn.WaitFor(); err != nil {
		return "", fmt.Errorf("run button not found")
	}

	// Small delay to ensure file attachment is processed by UI
	time.Sleep(1 * time.Second)

	if err := runBtn.Click(); err != nil {
		// Fallback
		page.Locator(SELECTOR_TEXTAREA).Press("Control+Enter")
	}

	// Wait for UI to become BUSY
	fmt.Println(">> Waiting for generation to START...")
	busyBtn := page.Locator(SELECTOR_RUN_BUSY).First()
	err = busyBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(10000),
	})
	if err != nil {
		return "", fmt.Errorf("UI failed to switch to 'generating' mode")
	}

	// Give capture a moment to start
	time.Sleep(500 * time.Millisecond)

	// Start monitoring accumulator
	fmt.Println(">> Monitoring capture during generation...")
	lastSize := 0
	accumulatorChan := make(chan int, 1)
	stopMonitoring := make(chan bool, 1)

	go func() {
		for {
			select {
			case <-stopMonitoring:
				return
			default:
				val, _ := page.Evaluate("window._AI_ACCUMULATOR")
				if text, ok := val.(string); ok {
					size := len(text)
					if size != lastSize {
						accumulatorChan <- size
						lastSize = size
					}
				}
				time.Sleep(200 * time.Millisecond)
			}
		}
	}()

	// Wait for UI to become IDLE
	fmt.Println(">> Waiting for generation to complete...")
	err = runBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(180000), // 3 mins timeout
	})
	if err != nil {
		stopMonitoring <- true
		return "", fmt.Errorf("timeout waiting for AI to finish")
	}

	fmt.Println(">> UI complete, capturing final chunks...")

	finalWait := time.After(5 * time.Second)
	lastUpdate := time.Now()

	for {
		select {
		case size := <-accumulatorChan:
			fmt.Printf(">> Captured: %d bytes\n", size)
			lastUpdate = time.Now()
		case <-finalWait:
			stopMonitoring <- true
			goto done
		default:
			if time.Since(lastUpdate) > 3*time.Second {
				stopMonitoring <- true
				goto done
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

done:
	fmt.Println(">> Capture monitoring stopped")

	val, err := page.Evaluate("window._AI_ACCUMULATOR")
	if err != nil {
		return "", fmt.Errorf("failed to read spy data: %v", err)
	}

	rawText, ok := val.(string)
	if !ok || rawText == "" {
		return "", fmt.Errorf("captured network data was empty")
	}

	return parseGoogleRPCResponse([]byte(rawText))
}

func buildPromptFromMessages(msgs []openai.ChatCompletionMessage) string {
	var sb strings.Builder
	for _, msg := range msgs {
		role := strings.ToUpper(msg.Role)
		sb.WriteString(fmt.Sprintf("%s: %s\n\n", role, msg.Content))
	}
	sb.WriteString("ASSISTANT: ")
	return sb.String()
}

func parseGoogleRPCResponse(body []byte) (string, error) {
	rawStr := string(body)
	var raw []interface{}
	err := json.Unmarshal([]byte(rawStr), &raw)

	if err != nil {
		openCount := strings.Count(rawStr, "[")
		closeCount := strings.Count(rawStr, "]")
		if openCount > closeCount {
			missing := openCount - closeCount
			fixedStr := rawStr + strings.Repeat("]", missing)
			err = json.Unmarshal([]byte(fixedStr), &raw)
		}
		if err != nil {
			return "", fmt.Errorf("could not parse response: %v", err)
		}
	}

	var textChunks []string
	findTextChunks(raw, &textChunks)

	if len(textChunks) == 0 {
		return "", fmt.Errorf("no text chunks found in response")
	}

	return strings.Join(textChunks, ""), nil
}

func findTextChunks(data interface{}, chunks *[]string) {
	switch v := data.(type) {
	case []interface{}:
		if len(v) == 2 {
			if v[0] == nil {
				if text, ok := v[1].(string); ok {
					if !strings.HasPrefix(text, "v1_") && text != "model" && len(text) > 0 {
						*chunks = append(*chunks, text)
					}
					return
				}
			}
		}
		for _, item := range v {
			findTextChunks(item, chunks)
		}
	}
}
