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
)

func main() {
	pm, err := services.InitAndConnect()
	if err != nil {
		log.Fatalf("Critical error during Playwright initialization: %v", err)
	}

	r := gin.Default()

	r.GET("/api/status", handleStatus)
	r.POST("/v1/chat/completions", HandleChatCompletions)
	r.POST("/v1/images/generations", HandleImageGenerations)

	srv := &http.Server{
		Addr:    ":8080",
		Handler: r,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	pm.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}
	log.Println("Server exiting")
}

func handleStatus(c *gin.Context) {
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
