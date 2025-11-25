package config

import (
	"log"
	"os"
	"strings"

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
	MaxFileSize int64 `mapstructure:"max_file_size"` // 最大文件大小(字节)
	ChunkSize   int   `mapstructure:"chunk_size"`    // 分块大小(字节)
	Enabled     bool  `mapstructure:"enabled"`       // 是否启用文件传输
}

type Runtime struct {
	GOMAXPROCS int `mapstructure:"gomaxprocs"` // 最大CPU核心数,0表示使用所有核心
}

func Load() *Config {
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
	viper.SetDefault("file_transfer.max_file_size", 524288000) // 500MB
	viper.SetDefault("file_transfer.chunk_size", 65536)        // 64KB
	viper.SetDefault("runtime.gomaxprocs", 0)                  // 使用所有核心
	viper.SetDefault("log.level", "info")
	viper.SetDefault("log.max_entries", 10000)
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
