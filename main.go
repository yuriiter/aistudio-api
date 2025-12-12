// main.go (Example Integration)

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aistudio-api/services"

	"github.com/gin-gonic/gin"
	"github.com/playwright-community/playwright-go"
)

func main() {
	// 1. Initialize Playwright and Chromium
	pm, err := services.InitAndConnect()
	if err != nil {
		log.Fatalf("Critical error during Playwright initialization: %v", err)
	}

	// 2. Set up Gin
	r := gin.Default()

	// Add routes
	r.GET("/api/status", handleStatus)
	r.POST("/api/generate", handleGenerate)

	srv := &http.Server{
		Addr:    ":8080",
		Handler: r,
	}

	// 3. Graceful Shutdown Setup
	// Run the server in a goroutine so it doesn't block the graceful shutdown code
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	// Wait for interrupt signal to gracefully shut down the server with a timeout
	quit := make(chan os.Signal, 1)
	// kill (no param) default send syscall.SIGTERM
	// kill -2 is syscall.SIGINT
	// kill -9 is syscall.SIGKILL (cannot be caught)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	// 4. Perform Playwright/Chromium cleanup before server shutdown
	pm.Cleanup()

	// The rest is standard graceful Gin shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}
	log.Println("Server exiting")
}

// Example Handlers
func handleStatus(c *gin.Context) {
	// A simple check to see if the browser is responsive
	browser := services.Manager.Browser
	if browser == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "Playwright Down"})
		return
	}

	pid := services.Manager.ChromeCmd.Process.Pid

	c.JSON(http.StatusOK, gin.H{
		"status":      "Running",
		"browser_pid": pid,
	})
}

func handleGenerate(c *gin.Context) {
	// Get a new page for this specific request
	page, err := services.Manager.CreateNewPage()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get browser page"})
		return
	}
	// IMPORTANT: Defer page close to clean up after the request
	defer page.Close()

	// Example Playwright action:
	_, err = page.Goto(services.NEW_PROMPT_PAGE, playwright.PageGotoOptions{
		Timeout: playwright.Float(0),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to navigate to prompt page"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Generation started", "url": page.URL()})
}
