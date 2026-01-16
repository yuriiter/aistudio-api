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

// ResponseCapture holds the captured network response
type ResponseCapture struct {
	mu       sync.Mutex
	chunks   []string
	complete bool
}

// Advanced JavaScript spy that handles the response directly
const NETWORK_SPY_SCRIPT = `
(() => {
	// Intercept XMLHttpRequest
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
			console.log('[Spy] Intercepting XHR GenerateContent');
			window._AI_CAPTURE_STARTED = true;
			window._AI_ACCUMULATOR = "";
			window._AI_CAPTURE_DONE = false;

			// Attach listener BEFORE send
			const xhr = this;
			let lastLength = 0;
			let captureTimeout = null;
			
			this.addEventListener('readystatechange', function() {
				// State 3 = LOADING (streaming data)
				if (this.readyState === 3) {
					try {
						const currentText = this.responseText || "";
						if (currentText.length > lastLength) {
							window._AI_ACCUMULATOR = currentText;
							lastLength = currentText.length;
							
							// Reset timeout - we're still getting data
							if (captureTimeout) {
								clearTimeout(captureTimeout);
							}
							// Mark as done if no new data comes for 2 seconds
							captureTimeout = setTimeout(() => {
								if (window._AI_ACCUMULATOR.length > 0 && !window._AI_CAPTURE_DONE) {
									window._AI_CAPTURE_DONE = true;
								}
							}, 2000);
						}
					} catch (e) {
						console.log('[Spy] Error reading responseText in state 3:', e);
					}
				}
				
				// State 4 = DONE
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
			
			this.addEventListener('error', function() {
				window._AI_CAPTURE_DONE = true;
			});
		}
		return originalXHRSend.apply(this, args);
	};

	// Also intercept fetch as backup
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

func ExecuteChatInteraction(req openai.ChatCompletionRequest) (string, error) {
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
	// New Input Method: Upload .txt file instead of typing
	// ---------------------------------------------------------
	fmt.Println(">> Preparing file upload...")

	// 1. Create a temporary .txt file with the prompt content
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
	defer os.Remove(tmpFilePath) // Cleanup temp file

	// 2. Locate the "Add Media" button
	addMediaBtn := page.Locator(SELECTOR_ADD_MEDIA_BTN).First()
	if err := addMediaBtn.WaitFor(); err != nil {
		return "", fmt.Errorf("add media button not found")
	}

	// 3. Handle File Chooser
	// We expect a file chooser to open after we click the button and then select "UploadFiles"
	fmt.Println(">> Triggering file chooser...")
	fileChooser, err := page.ExpectFileChooser(func() error {
		// Open the menu
		if err := addMediaBtn.Click(); err != nil {
			return err
		}
		// Click the specific text in the menu to trigger system dialog
		// Using GetByText as requested
		return page.GetByText(SELECTOR_UPLOAD_TEXT).Click()
	})
	if err != nil {
		return "", fmt.Errorf("failed to open file chooser: %v", err)
	}

	// 4. Set the file
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

	// Small delay to ensure file processing in UI is registered before clicking Run
	time.Sleep(1 * time.Second)

	if err := runBtn.Click(); err != nil {
		// Fallback if click fails
		page.Locator(SELECTOR_TEXTAREA).Press("Control+Enter")
	}

	// Wait for UI to become BUSY (generation started)
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

	// Start monitoring accumulator DURING generation
	fmt.Println(">> Monitoring capture during generation...")
	lastSize := 0
	accumulatorChan := make(chan int, 1)
	stopMonitoring := make(chan bool, 1)

	// Monitor in background
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

	// Wait for UI to become IDLE (generation finished)
	fmt.Println(">> Waiting for generation to complete...")
	err = runBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(120000), // Wait up to 2 mins
	})
	if err != nil {
		stopMonitoring <- true
		return "", fmt.Errorf("timeout waiting for AI to finish")
	}

	fmt.Println(">> UI complete, capturing final chunks...")

	// Continue monitoring for a bit longer after UI completes
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
			// If no updates for 3 seconds after UI complete, we're done
			if time.Since(lastUpdate) > 3*time.Second {
				stopMonitoring <- true
				goto done
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

done:
	fmt.Println(">> Capture monitoring stopped")

	// Read final captured data
	val, err := page.Evaluate("window._AI_ACCUMULATOR")
	if err != nil {
		return "", fmt.Errorf("failed to read spy data: %v", err)
	}

	rawText, ok := val.(string)
	if !ok || rawText == "" {
		return "", fmt.Errorf("captured network data was empty (length: 0)")
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func parseGoogleRPCResponse(body []byte) (string, error) {
	rawStr := string(body)
	var raw []interface{}
	err := json.Unmarshal([]byte(rawStr), &raw)

	if err != nil {
		// Try to fix incomplete JSON by adding closing brackets
		openCount := strings.Count(rawStr, "[")
		closeCount := strings.Count(rawStr, "]")

		if openCount > closeCount {
			missing := openCount - closeCount
			fixedStr := rawStr + strings.Repeat("]", missing)

			err = json.Unmarshal([]byte(fixedStr), &raw)
			if err != nil {
				return "", fmt.Errorf("could not parse response: %v", err)
			}
		} else {
			return "", fmt.Errorf("could not parse response: %v", err)
		}
	}

	// Extract all text chunks
	var textChunks []string
	findTextChunks(raw, &textChunks)

	if len(textChunks) == 0 {
		return "", fmt.Errorf("no text chunks found in response")
	}

	// Concatenate all chunks
	fullText := strings.Join(textChunks, "")
	return fullText, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// findTextChunks recursively finds all arrays of form [null, "text"] and collects the text
func findTextChunks(data interface{}, chunks *[]string) {
	switch v := data.(type) {
	case []interface{}:
		// Check if this is a [null, string] pair
		if len(v) == 2 {
			if v[0] == nil {
				if text, ok := v[1].(string); ok {
					// Filter out metadata
					if !strings.HasPrefix(text, "v1_") && text != "model" && len(text) > 0 {
						*chunks = append(*chunks, text)
					}
					return // Don't recurse into this array
				}
			}
		}
		// Recurse into all elements
		for _, item := range v {
			findTextChunks(item, chunks)
		}
	}
}
