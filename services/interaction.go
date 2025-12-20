package services

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"aistudio-api/openai"
	"github.com/playwright-community/playwright-go"
)

// Selectors
const (
	SELECTOR_TEXTAREA = "textarea"
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
				console.log('[Spy] ReadyState:', this.readyState, 'Status:', this.status);
				
				// State 3 = LOADING (streaming data)
				if (this.readyState === 3) {
					try {
						const currentText = this.responseText || "";
						if (currentText.length > lastLength) {
							window._AI_ACCUMULATOR = currentText;
							lastLength = currentText.length;
							console.log('[Spy] Streaming... accumulated', lastLength, 'bytes');
							
							// Reset timeout - we're still getting data
							if (captureTimeout) {
								clearTimeout(captureTimeout);
							}
							// Mark as done if no new data comes for 2 seconds
							captureTimeout = setTimeout(() => {
								if (window._AI_ACCUMULATOR.length > 0 && !window._AI_CAPTURE_DONE) {
									console.log('[Spy] No new data for 2s, marking complete with', window._AI_ACCUMULATOR.length, 'bytes');
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
					console.log('[Spy] XHR Complete, status:', this.status);
					console.log('[Spy] Response length:', this.responseText ? this.responseText.length : 0);
					window._AI_ACCUMULATOR = this.responseText || "";
					window._AI_CAPTURE_DONE = true;
					console.log('[Spy] Captured', window._AI_ACCUMULATOR.length, 'bytes');
				}
				
				// State 0 = UNSENT/ABORTED - if we have data, wait a bit then mark as done
				if (this.readyState === 0 && window._AI_ACCUMULATOR.length > 0) {
					if (captureTimeout) clearTimeout(captureTimeout);
					console.log('[Spy] XHR aborted, we have', window._AI_ACCUMULATOR.length, 'bytes');
					// Don't mark as done immediately - give it a moment
					setTimeout(() => {
						if (!window._AI_CAPTURE_DONE) {
							console.log('[Spy] Finalizing after abort');
							window._AI_CAPTURE_DONE = true;
						}
					}, 500);
				}
			});
			
			// Also try using onload as backup
			this.addEventListener('load', function() {
				console.log('[Spy] XHR Load event fired');
				if (!window._AI_CAPTURE_DONE) {
					window._AI_ACCUMULATOR = this.responseText || "";
					window._AI_CAPTURE_DONE = true;
					console.log('[Spy] Captured via load event:', window._AI_ACCUMULATOR.length, 'bytes');
				}
			});
			
			// Handle errors
			this.addEventListener('error', function() {
				console.log('[Spy] XHR Error event');
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
			console.log("[Spy] Intercepting Fetch GenerateContent");
			window._AI_CAPTURE_STARTED = true;
			window._AI_ACCUMULATOR = ""; 
			window._AI_CAPTURE_DONE = false;
			
			const clone = response.clone();
			const text = await clone.text();
			
			window._AI_ACCUMULATOR = text;
			window._AI_CAPTURE_DONE = true;
			console.log('[Spy] Captured', text.length, 'bytes via fetch');
		}
		return response;
	};
	
	console.log("[Spy] Network interceptors installed (XHR + Fetch)");
})();
`

func ExecuteChatInteraction(req openai.ChatCompletionRequest) (string, error) {
	page, err := Manager.CreateNewPage()
	if err != nil {
		return "", err
	}
	defer page.Close()

	// // Enable console logging from the page
	// page.OnConsole(func(msg playwright.ConsoleMessage) {
	// 	fmt.Printf(">> [Browser Console] %s: %s\n", msg.Type(), msg.Text())
	// })

	// Inject the spy script
	// fmt.Println(">> Injecting network spy (XHR + Fetch)...")
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

	// Input
	fmt.Println(">> Waiting for input...")
	inputLocator := page.Locator(SELECTOR_TEXTAREA).First()
	if err := inputLocator.WaitFor(); err != nil {
		return "", fmt.Errorf("input not found")
	}

	fullPrompt := buildPromptFromMessages(req.Messages)
	if err := inputLocator.Fill(fullPrompt); err != nil {
		return "", err
	}

	// Click Run (IDLE state)
	fmt.Println(">> Clicking Run...")
	runBtn := page.Locator(SELECTOR_RUN_IDLE).First()
	if err := runBtn.WaitFor(); err != nil {
		return "", fmt.Errorf("run button not found")
	}

	if err := runBtn.Click(); err != nil {
		inputLocator.Press("Control+Enter")
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

	// Verify capture started
	started, _ := page.Evaluate("window._AI_CAPTURE_STARTED")
	if isStarted, ok := started.(bool); !ok || !isStarted {
		fmt.Println(">> WARNING: Capture may not have started")
	} else {
		fmt.Println(">> Network capture confirmed started")
	}

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

	// fmt.Printf(">> Final capture: %d bytes\n", len(rawText))

	// Check if JSON looks complete
	trimmed := strings.TrimSpace(rawText)
	if !strings.HasSuffix(trimmed, "]") && !strings.HasSuffix(trimmed, "}") {
		// fmt.Printf(">> WARNING: Response may be incomplete\n")
		// fmt.Printf(">> Last 100 chars: ...%s\n", trimmed[max(0, len(trimmed)-100):])
		// Don't fail, just try to parse what we have
	}

	// Optional: Print preview for debugging
	// if len(rawText) < 500 {
	// 	fmt.Printf(">> Full response:\n%s\n", rawText)
	// } else {
	// 	fmt.Printf(">> Preview (first 200 chars): %s...\n", rawText[:200])
	// 	fmt.Printf(">> Preview (last 200 chars): ...%s\n", rawText[len(rawText)-200:])
	//
	// 	// Also save to file for inspection
	// 	os.WriteFile("/tmp/google_response.txt", []byte(rawText), 0644)
	// 	fmt.Println(">> Full response saved to /tmp/google_response.txt")
	// }

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

	// fmt.Printf(">> Parser: received %d bytes\n", len(rawStr))

	// The response might be incomplete, try to parse what we can
	// First, try to parse as-is
	var raw []interface{}
	err := json.Unmarshal([]byte(rawStr), &raw)

	if err != nil {
		fmt.Printf(">> Parser: direct parse failed: %v\n", err)

		// Try to fix incomplete JSON by adding closing brackets
		// Count opening vs closing brackets
		openCount := strings.Count(rawStr, "[")
		closeCount := strings.Count(rawStr, "]")

		if openCount > closeCount {
			missing := openCount - closeCount
			fmt.Printf(">> Parser: missing %d closing brackets, attempting fix\n", missing)
			fixedStr := rawStr + strings.Repeat("]", missing)

			err = json.Unmarshal([]byte(fixedStr), &raw)
			if err != nil {
				fmt.Printf(">> Parser: fix attempt failed: %v\n", err)
				return "", fmt.Errorf("could not parse response: %v", err)
			}
			fmt.Println(">> Parser: successfully parsed after adding closing brackets")
		} else {
			return "", fmt.Errorf("could not parse response: %v", err)
		}
	} else {
		fmt.Println(">> Parser: parsed successfully on first try")
	}

	// Extract all text chunks
	var textChunks []string
	findTextChunks(raw, &textChunks)

	fmt.Printf(">> Parser: extracted %d text chunks\n", len(textChunks))

	if len(textChunks) == 0 {
		return "", fmt.Errorf("no text chunks found in response")
	}

	// Show first few chunks
	for i, chunk := range textChunks {
		if i < 3 {
			preview := chunk
			if len(preview) > 60 {
				preview = preview[:60] + "..."
			}
			// fmt.Printf(">> Parser: chunk %d: %q\n", i, preview)
		}
	}

	// Concatenate all chunks
	fullText := strings.Join(textChunks, "")
	// fmt.Printf(">> Parser: final result: %d chars\n", len(fullText))

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
