package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 顶层配置。
type Config struct {
	Listen       string `json:"listen"`
	APIKey       string `json:"-"` // 只读 env CP2A_API_KEY
	AuthDir      string `json:"auth_dir"`
	StateFile    string `json:"state_file"`
	DefaultModel string `json:"default_model"`

	Cooldown struct {
		SoftRate    string `json:"soft_rate"`
		ErrThresh   int    `json:"err_threshold"`
		ErrCooldown string `json:"err_cooldown"`
	} `json:"cooldown"`

	Quota struct {
		Enabled             bool   `json:"enabled"`
		PollMinutes         int    `json:"poll_minutes"`
		ApplyThreshold      int64  `json:"apply_threshold"`
		ApplyMethod         string `json:"apply_method"`
		ApplyCooldownHour   int    `json:"apply_cooldown_hours"`
		RegisterOnStart     bool   `json:"register_on_start"`
		AutoRenew           bool   `json:"auto_renew"`
		RenewThresholdHours int    `json:"renew_threshold_hours"` // 默认 6
	} `json:"quota"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	} `json:"upstream"`

	// 解析后的 duration
	SoftRateDur    time.Duration
	ErrCooldownDur time.Duration
}

// Default 默认配置。裸机默认只监听 loopback；Docker 由 compose 显式覆盖为 :7867。
func Default() *Config {
	c := &Config{
		Listen:       "127.0.0.1:7867",
		AuthDir:      "./auths",
		StateFile:    "./data/state.json",
		DefaultModel: "glm-5.2",
	}
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	c.Quota.Enabled = true
	c.Quota.PollMinutes = 10
	c.Quota.ApplyThreshold = 50
	c.Quota.ApplyMethod = "register"
	c.Quota.ApplyCooldownHour = 6
	c.Quota.RegisterOnStart = true
	c.Quota.AutoRenew = true
	c.Quota.RenewThresholdHours = 6
	c.Upstream.TimeoutSeconds = 120
	return c
}

// Load 加载配置 + CP2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config: %w", err)
			}
		} else if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	if err := applyEnv(c); err != nil {
		return nil, err
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) error {
	if v, ok := os.LookupEnv("CP2A_API_KEY"); ok {
		c.APIKey = v
	}
	if v := os.Getenv("CP2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("CP2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v, ok := os.LookupEnv("CP2A_STATE_FILE"); ok {
		c.StateFile = v
	}
	if v := os.Getenv("CP2A_DEFAULT_MODEL"); v != "" {
		c.DefaultModel = v
	}
	if v := os.Getenv("CP2A_QUOTA_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("CP2A_QUOTA_ENABLED: %w", err)
		}
		c.Quota.Enabled = b
	}
	if v := os.Getenv("CP2A_QUOTA_POLL_MINUTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("CP2A_QUOTA_POLL_MINUTES: %w", err)
		}
		c.Quota.PollMinutes = n
	}
	if v := os.Getenv("CP2A_QUOTA_THRESHOLD"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("CP2A_QUOTA_THRESHOLD: %w", err)
		}
		c.Quota.ApplyThreshold = n
	}
	if v := os.Getenv("CP2A_QUOTA_METHOD"); v != "" {
		c.Quota.ApplyMethod = v
	}
	if v := os.Getenv("CP2A_QUOTA_COOLDOWN_HOURS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("CP2A_QUOTA_COOLDOWN_HOURS: %w", err)
		}
		c.Quota.ApplyCooldownHour = n
	}
	if v := os.Getenv("CP2A_QUOTA_REGISTER_ON_START"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("CP2A_QUOTA_REGISTER_ON_START: %w", err)
		}
		c.Quota.RegisterOnStart = b
	}
	if v := os.Getenv("CP2A_QUOTA_AUTO_RENEW"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("CP2A_QUOTA_AUTO_RENEW: %w", err)
		}
		c.Quota.AutoRenew = b
	}
	if v := os.Getenv("CP2A_QUOTA_RENEW_THRESHOLD_HOURS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("CP2A_QUOTA_RENEW_THRESHOLD_HOURS: %w", err)
		}
		c.Quota.RenewThresholdHours = n
	}
	if v := os.Getenv("CP2A_UPSTREAM_TIMEOUT_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("CP2A_UPSTREAM_TIMEOUT_SECONDS: %w", err)
		}
		c.Upstream.TimeoutSeconds = n
	}
	return nil
}

func (c *Config) normalize() error {
	c.APIKey = strings.TrimSpace(c.APIKey)
	if c.APIKey == "" {
		return fmt.Errorf("CP2A_API_KEY is required; generate one with `openssl rand -hex 24`")
	}
	switch strings.ToLower(c.APIKey) {
	case "changeme", "change-me", "your-api-key-here", "dummy-key-for-catpaw":
		return fmt.Errorf("CP2A_API_KEY uses an insecure example value; generate a unique random key")
	}

	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "auto"
	}
	c.Listen = strings.TrimSpace(c.Listen)
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7867"
	}
	if !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	if _, port, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	} else {
		n, convErr := strconv.Atoi(port)
		if convErr != nil || n < 1 || n > 65535 {
			return fmt.Errorf("listen %q: port must be numeric in range 1..65535", c.Listen)
		}
	}
	if strings.TrimSpace(c.AuthDir) == "" {
		return fmt.Errorf("auth_dir must not be empty")
	}
	if c.Quota.ApplyThreshold <= 0 {
		c.Quota.ApplyThreshold = 50
	}
	if c.Quota.PollMinutes <= 0 {
		c.Quota.PollMinutes = 10
	}
	if c.Quota.ApplyCooldownHour <= 0 {
		c.Quota.ApplyCooldownHour = 6
	}
	if c.Quota.RenewThresholdHours <= 0 {
		c.Quota.RenewThresholdHours = 6
	}
	if c.Quota.ApplyMethod == "" {
		c.Quota.ApplyMethod = "register"
	}
	switch c.Quota.ApplyMethod {
	case "register", "campaign", "none":
	default:
		return fmt.Errorf("quota.apply_method must be register, campaign, or none, got %q", c.Quota.ApplyMethod)
	}
	return nil
}
