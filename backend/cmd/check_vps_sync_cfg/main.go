package main

import (
	"fmt"
	"os"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func main() {
	cfg, err := config.LoadForBootstrap()
	if err != nil {
		// try Load
		cfg, err = config.Load()
	}
	if err != nil {
		fmt.Println("load error:", err)
		// still try to print env
		fmt.Println("env ENABLED=", os.Getenv("TOKEN_REFRESH_ANTHROPIC_SETUP_TOKEN_VPS_SYNC_ENABLED"))
		os.Exit(1)
	}
	tr := cfg.TokenRefresh
	fmt.Printf("TokenRefresh.Enabled=%v\n", tr.Enabled)
	fmt.Printf("TokenRefresh.RequestRefreshEnabled=%v\n", tr.RequestRefreshEnabled)
	fmt.Printf("TokenRefresh.RefreshBeforeExpiryHours=%v\n", tr.RefreshBeforeExpiryHours)
	fmt.Printf("AnthropicSetupTokenVPSSyncEnabled=%v\n", tr.AnthropicSetupTokenVPSSyncEnabled)
	fmt.Printf("AnthropicSetupTokenVPSSyncCommand=%q\n", tr.AnthropicSetupTokenVPSSyncCommand)
	fmt.Printf("AnthropicSetupTokenVPSSyncDebounceSeconds=%d\n", tr.AnthropicSetupTokenVPSSyncDebounceSeconds)
	fmt.Printf("Server.Port=%d\n", cfg.Server.Port)
	fmt.Printf("Log.Level=%s Service=%s\n", cfg.Log.Level, cfg.Log.ServiceName)
}
