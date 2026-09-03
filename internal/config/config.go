package config

import (
	"log"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/spf13/viper"
)

type Config struct {
	Mode         string       `mapstructure:"mode"`
	Server       Server       `mapstructure:"server"`
	TLS          TLS          `mapstructure:"tls"`
	CORS         CORS         `mapstructure:"cors"`
	Log          Log          `mapstructure:"log"`
	WebSocket    WebSocket    `mapstructure:"websocket"`
	FileTransfer FileTransfer `mapstructure:"file_transfer"`
	Runtime      Runtime      `mapstructure:"runtime"`
	GLM          GLM          `mapstructure:"glm"`
	JWT          JWT          `mapstructure:"jwt"`
	TURN         TURN         `mapstructure:"turn"`
}

type Server struct {
	Port string `mapstructure:"port"`
}

type TLS struct {
	Enabled  bool   `mapstructure:"enabled"`
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`
	AutoCert bool   `mapstructure:"auto_cert"`
	Domain   string `mapstructure:"domain"`
}

type CORS struct {
	AllowedOrigins []string `mapstructure:"allowed_origins"`
}

type Log struct {
	Level      string `mapstructure:"level"`
	MaxEntries int    `mapstructure:"max_entries"`
}

type WebSocket struct {
	MaxRoomUsers int `mapstructure:"max_room_users"`
}

type FileTransfer struct {
	MaxFileSize   int64  `mapstructure:"max_file_size"`    // PRO 统一上限(字节), 默认 3GB
	ChunkSize     int    `mapstructure:"chunk_size"`       // 分块大小(字节)
	Enabled       bool   `mapstructure:"enabled"`          // 是否启用文件传输
	ProInviteCode string `mapstructure:"pro_invite_code"`  // PRO 邀请码
}

type JWT struct {
	Secret          string `mapstructure:"secret"`
	ExpirationHours int    `mapstructure:"expiration_hours"`
}

// TURN 短效凭据签发配置（RFC 5766 use-auth-secret 模式）。
// Enabled 同时控制凭据端点与嵌入式 pion/turn 中继服务。
type TURN struct {
	// Enabled 是否启用 TURN（凭据端点 + 嵌入式中继）。false 时 /api/turn-credentials 返回 404 且不启动中继。
	Enabled bool `mapstructure:"enabled"`
	// Secret 与签发/校验共用的 static-auth-secret。
	// 绝不进 git：通过 LETSHARE_TURN_SECRET 环境变量或服务器配置文件注入。
	Secret string `mapstructure:"secret"`
	// URIs coturn/pion 服务器地址列表（下发前端 iceServers 用）。
	URIs []string `mapstructure:"uris"`
	// TTLSeconds 短效凭据有效期（秒）。
	TTLSeconds int `mapstructure:"ttl_seconds"`
	// PublicIP 中继下发给客户端的公网 IP（RelayAddress；本地默认 127.0.0.1）。
	PublicIP string `mapstructure:"public_ip"`
	// Port 嵌入式 TURN listener 端口（UDP+TCP 同端口，默认 3478）。
	Port int `mapstructure:"port"`
	// RelayPortMin / RelayPortMax 中继端口段（含两端，默认 49160-49200）。
	RelayPortMin int `mapstructure:"relay_port_min"`
	RelayPortMax int `mapstructure:"relay_port_max"`
	// Realm TURN realm（默认 letshare.fun）。
	Realm string `mapstructure:"realm"`
}

type Runtime struct {
	GOMAXPROCS int `mapstructure:"gomaxprocs"` // 最大CPU核心数,0表示使用所有核心
}

type GLM struct {
	APIKey      string `mapstructure:"api_key"`
	ModelOpus   string `mapstructure:"model_opus"`
	ModelSonnet string `mapstructure:"model_sonnet"`
	ModelVision string `mapstructure:"model_vision"`
	BaseURL     string `mapstructure:"base_url"`
}

func Load() *Config {
	_ = godotenv.Load()

	// 确定运行模式
	mode := os.Getenv("MODE")
	if mode == "" {
		mode = "production" // 默认生产模式
	}

	viper.SetConfigName(mode)
	viper.SetConfigType("yaml")
	viper.AddConfigPath("./configs")

	// 设置环境变量前缀
	viper.SetEnvPrefix("LETSHARE")
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// 设置默认值
	setDefaults()

	if err := viper.ReadInConfig(); err != nil {
		log.Printf("配置文件读取失败: %v，使用默认配置", err)
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		log.Fatalf("配置解析失败: %v", err)
	}

	cfg.Mode = mode

	return &cfg
}

func setDefaults() {
	viper.SetDefault("server.port", "8080")
	viper.SetDefault("websocket.max_room_users", 10)
	viper.SetDefault("file_transfer.enabled", true)
	viper.SetDefault("file_transfer.max_file_size", 3221225472) // 3GB — PRO 统一上限
	viper.SetDefault("file_transfer.chunk_size", 65536)         // 64KB
	viper.SetDefault("file_transfer.pro_invite_code", "bigonion")
	viper.SetDefault("jwt.secret", "letshare-jwt-secret-key-2024")
	viper.SetDefault("jwt.expiration_hours", 720) // 30 天
	viper.SetDefault("turn.enabled", false)
	viper.SetDefault("turn.secret", os.Getenv("LETSHARE_TURN_SECRET"))
	viper.SetDefault("turn.uris", []string{"turn:ecs.letshare.fun:3478?transport=udp", "turn:ecs.letshare.fun:3478?transport=tcp"})
	viper.SetDefault("turn.ttl_seconds", 3600) // 默认 1 小时；前端在到期前 60s 主动续期 + 按需 ICE restart（3.7.0）
	viper.SetDefault("turn.public_ip", "127.0.0.1") // 本地默认；生产须配服务器公网 IP
	viper.SetDefault("turn.port", 3478)             // TURN/STUN listener（UDP+TCP 同端口）
	viper.SetDefault("turn.relay_port_min", 49160)  // 中继端口段下界
	viper.SetDefault("turn.relay_port_max", 49200)  // 中继端口段上界
	viper.SetDefault("turn.realm", "letshare.fun")
	viper.SetDefault("runtime.gomaxprocs", 0)                  // 使用所有核心
	viper.SetDefault("glm.base_url", "https://open.bigmodel.cn/api/paas/v4")
	viper.SetDefault("glm.model_opus", os.Getenv("GLM_MODEL_OPUS"))
	viper.SetDefault("glm.model_sonnet", os.Getenv("GLM_MODEL_SONNET"))
	viper.SetDefault("glm.model_vision", os.Getenv("GLM_MODEL_VISION"))
	viper.SetDefault("glm.api_key", os.Getenv("GLM_API_KEY"))
	viper.SetDefault("tls.enabled", false)
	viper.SetDefault("tls.cert_file", "/etc/letsencrypt/live/ecs.letshare.fun/fullchain.pem")
	viper.SetDefault("tls.key_file", "/etc/letsencrypt/live/ecs.letshare.fun/privkey.pem")
	viper.SetDefault("tls.auto_cert", true)
	viper.SetDefault("tls.domain", "ecs.letshare.fun")
	viper.SetDefault("cors.allowed_origins", []string{
		"https://letshare.fun",
		"https://www.letshare.fun",
		"https://cdn.letshare.fun",
		"http://localhost:3000",
		"http://localhost:5173",
	})
	viper.SetDefault("log.level", "info")
	viper.SetDefault("log.max_entries", 200)
	viper.SetDefault("websocket.max_room_users", 50)
}
