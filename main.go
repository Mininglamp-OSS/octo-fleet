// octo-fleet — runtime/bot orchestration service split out of octo-server.
//
// Standalone Go binary that owns:
//   - agent_runtime registry (daemons + their detected runtimes)
//   - bot CRUD (orchestration metadata only — bot_token stays on octo-server)
//   - bot.provision command dispatch via daemon heartbeat
//
// Auth: fleet calls octo-server /v1/auth/verify-* endpoints (合并 plan
// 决策一+二). 旧 JWT/JWKS 验签链路已删 (Phase 2 起).
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Mininglamp-OSS/octo-fleet/internal/auth"
	_ "github.com/Mininglamp-OSS/octo-fleet/internal"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/module"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/gin-gonic/gin"
	"github.com/judwhite/go-svc"
	"github.com/spf13/viper"
)

// build-time ldflags
var (
	Version    string
	Commit     string
	CommitDate string
	TreeState  string
)

func loadConfigFromFile(cfgFile string) *viper.Viper {
	vp := viper.New()
	vp.SetConfigFile(cfgFile)
	if err := vp.ReadInConfig(); err != nil {
		panic(fmt.Sprintf("Failed to load config file %s: %v", cfgFile, err))
	}
	fmt.Println("Using config file:", vp.ConfigFileUsed())
	return vp
}

// @title           Octo Fleet API
// @version         1.0.0
// @description     Runtime & bot orchestration for OCTO — daemon registry, heartbeat dispatch, bot provisioning. Gateway mounts this at <host>/fleet/api/; the spec describes only /v1/<resource> (A.1).
// @BasePath        /v1
// @contact.name    OCTO Team (Mininglamp-OSS)
// @contact.url     https://github.com/Mininglamp-OSS/octo-fleet
//
// @tag.name        runtime
// @tag.description Agent runtime registry — register, heartbeat, deregister, list, delete.
// @tag.name        bot
// @tag.description Bot orchestration — create, mint, provision, ack, archive.
// @tag.name        upgrade
// @tag.description Component / daemon upgrade tasks.
// @tag.name        provider
// @tag.description Runtime-provider catalog.
// @tag.name        event
// @tag.description Daemon SSE reverse-dispatch stream.
//
// @securityDefinitions.apikey Bearer
// @in              header
// @name            Authorization
func main() {
	var cfgFile string
	flag.StringVar(&cfgFile, "config", "configs/fleet.yaml", "config file")
	flag.Parse()

	vp := loadConfigFromFile(cfgFile)
	vp.SetEnvPrefix("FLEET")
	vp.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	vp.AutomaticEnv()

	gin.SetMode(gin.ReleaseMode)

	cfg := config.New()
	cfg.Version = Version
	cfg.ConfigureWithViper(vp)

	ctx := config.NewContext(cfg)

	logOpts := log.NewOptions()
	logOpts.Level = cfg.Logger.Level
	logOpts.LineNum = cfg.Logger.LineNum
	logOpts.LogDir = cfg.Logger.Dir
	log.Configure(logOpts)

	// Auth middleware singleton (合并 plan §4): fleet calls server's
	// verify-* endpoints to authenticate user / bot / api_key callers.
	// 合并 plan 决策一+二 Phase 4: 改用专用字段 auth.octoServerURL.
	// 仍 fallback 旧字段 auth.serverJwksURL (反推 base URL) 兼容未更新的
	// fleet.yaml. **下个发版前删除 serverJwksURL fallback** (运维需保证
	// fleet.yaml 已更新到 octoServerURL).
	octoServerURL := vp.GetString("auth.octoServerURL")
	if octoServerURL == "" {
		jwksURL := vp.GetString("auth.serverJwksURL")
		if jwksURL == "" {
			octoServerURL = "http://localhost:8090"
		} else {
			octoServerURL = strings.TrimSuffix(jwksURL, "/.well-known/jwks.json")
		}
	}
	auth.Initialize(auth.Config{OctoIMURL: octoServerURL})

	// octo-fleet is API-only — no grpc, no message worker, no cron events.
	runAPI(ctx)
}

func runAPI(ctx *config.Context) {
	s := server.New(ctx)
	ctx.SetHttpRoute(s.GetRoute())

	// Modules register themselves via init() — see internal/modules.go.
	if err := module.Setup(ctx); err != nil {
		panic(err)
	}

	printServerInfo(ctx)

	if err := svc.Run(s); err != nil {
		fmt.Fprintln(os.Stderr, "fleet svc exit:", err)
		os.Exit(1)
	}
}

func printServerInfo(ctx *config.Context) {
	fmt.Println("==========================================")
	fmt.Println("  octo-fleet — runtime/bot orchestration  ")
	fmt.Println("==========================================")
	fmt.Println("Version:    ", Version)
	fmt.Println("Commit:     ", Commit)
	fmt.Println("CommitDate: ", CommitDate)
	fmt.Println("Listen:     ", ctx.GetConfig().Addr)
	fmt.Println("==========================================")
}
