package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"ccg/internal/config"
	"ccg/internal/logger"
)

type CLIType string

const (
	CLITypeClaude CLIType = "claude"
	CLITypeCodex  CLIType = "codex"
	CLITypeGemini CLIType = "gemini"
)

type CLIConfigBackup struct {
	OriginalContent string
	FilePath        string
	Exists          bool
	backupTime      string
}

type CLIConfigService struct {
	config       *config.Config
	backups      map[CLIType]CLIConfigBackup
	backupsMutex sync.RWMutex
	modified     map[CLIType]bool
	modifiedMu   sync.RWMutex
	isContainer  bool
}

func NewCLIConfigService(cfg *config.Config) *CLIConfigService {
	isContainer := checkIfContainer()
	if isContainer {
		logger.Info("Running in container environment")
	}
	return &CLIConfigService{
		config:      cfg,
		backups:     make(map[CLIType]CLIConfigBackup),
		modified:    make(map[CLIType]bool),
		isContainer: isContainer,
	}
}

func (s *CLIConfigService) ValidateContainerSetup() []error {
	var errors []error

	configPaths := []struct {
		name string
		path string
	}{
		{"Claude", "~/.claude/settings.json"},
		{"Codex", "~/.codex/config.toml"},
		{"Gemini", "~/.gemini/settings.json"},
	}

	if s.isContainer {
		logger.Info("Container environment detected, checking for crashed sessions...")
		for _, cp := range configPaths {
			expandedPath := cp.path
			if strings.HasPrefix(cp.path, "~/") {
				homeDir, err := s.getHomeDir()
				if err != nil {
					errors = append(errors, fmt.Errorf("%s: cannot determine home directory: %w", cp.name, err))
					continue
				}
				expandedPath = filepath.Join(homeDir, cp.path[2:])
			}

			backupFile := expandedPath + ".ccg.original"
			if _, err := os.Stat(backupFile); err == nil {
				logger.Warn("Found uncleared backup from previous session: %s", backupFile)
				logger.Warn("Previous session may have crashed. Restoring original config...")

				originalContent, err := os.ReadFile(backupFile)
				if err != nil {
					logger.Error("Failed to read backup file: %v", err)
					continue
				}

				configFile := expandedPath
				if err := os.WriteFile(configFile, originalContent, 0644); err != nil {
					logger.Error("Failed to restore config file: %v", err)
					continue
				}

				logger.Info("Restored %s config from backup", cp.name)

				if err := os.Remove(backupFile); err != nil {
					logger.Warn("Failed to remove backup file: %v", err)
				}
			}
		}
	}

	for _, cp := range configPaths {
		expandedPath := cp.path
		if strings.HasPrefix(cp.path, "~/") {
			homeDir, err := s.getHomeDir()
			if err != nil {
				errors = append(errors, fmt.Errorf("%s: cannot determine home directory: %w", cp.name, err))
				continue
			}
			expandedPath = filepath.Join(homeDir, cp.path[2:])
		}

		dir := filepath.Dir(expandedPath)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			errors = append(errors, fmt.Errorf("%s: directory not mounted: %s", cp.name, dir))
		} else if !s.isDirWritable(dir) {
			errors = append(errors, fmt.Errorf("%s: directory not writable: %s", cp.name, dir))
		}
	}

	proxyHost := getContainerProxyHost()
	info := s.GetContainerInfo()

	logger.Info("========================================")
	logger.Info("     Container Environment Info")
	logger.Info("========================================")
	logger.Info("  OS/Arch:       %s/%s", runtime.GOOS, runtime.GOARCH)
	logger.Info("  Hostname:     %s", info["hostname"])
	logger.Info("  Proxy:        %s:%s", proxyHost, info["proxy_port"])
	logger.Info("  Docker:       %s", info["docker_available"])
	logger.Info("  Kubernetes:   %s", info["kubernetes"])
	logger.Info("  Home Dir:     %s", info["home_dir"])
	logger.Info("========================================")

	if len(errors) > 0 {
		logger.Warn("  ✗ Volume mount issues detected:")
		for _, e := range errors {
			logger.Warn("    - %v", e)
		}
	} else {
		logger.Info("  ✓ All required volumes mounted correctly")
	}
	logger.Info("========================================")

	return errors
}

func checkIfContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if os.Getenv("container") != "" {
		return true
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	if runtime.GOOS == "windows" {
		if os.Getenv("DOTNET_RUNNING_IN_CONTAINER") == "true" {
			return true
		}
		if os.Getenv("NATIVEPARENTPROCESSID") != "" {
			return true
		}
	}
	return false
}

func (s *CLIConfigService) getHomeDir() (string, error) {
	if s.isContainer {
		if home := os.Getenv("HOME"); home != "" {
			return home, nil
		}
		if home := os.Getenv("USERPROFILE"); home != "" {
			return home, nil
		}
		if home := os.Getenv("HOMEPATH"); home != "" {
			if drive := os.Getenv("HOMEDRIVE"); drive != "" {
				return filepath.Join(drive, home), nil
			}
			return home, nil
		}
		if runtime.GOOS == "windows" {
			if appData := os.Getenv("APPDATA"); appData != "" {
				return appData, nil
			}
		}
		return "", fmt.Errorf("cannot determine home directory in container environment")
	}
	return os.UserHomeDir()
}

func (s *CLIConfigService) GetContainerInfo() map[string]string {
	info := make(map[string]string)
	info["is_container"] = fmt.Sprintf("%v", s.isContainer)
	info["os"] = runtime.GOOS
	info["arch"] = runtime.GOARCH
	info["proxy_host"] = getContainerProxyHost()
	info["proxy_port"] = fmt.Sprintf("%d", s.config.GetPort())

	if s.isContainer {
		if home, err := s.getHomeDir(); err == nil {
			info["home_dir"] = home
		}
		if hostname, err := os.Hostname(); err == nil {
			info["hostname"] = hostname
		}
		info["docker_available"] = s.isFileExists("/.dockerenv")
		if runtime.GOOS == "windows" {
			if _, err := os.Stat("C:\\Program Files\\Docker\\Docker\\resources"); err == nil {
				info["docker_available"] = "true"
			}
		}
		info["kubernetes"] = "false"
		if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
			info["kubernetes"] = "true"
		}
	}
	return info
}

func (s *CLIConfigService) isFileExists(path string) string {
	if _, err := os.Stat(path); err == nil {
		return "true"
	}
	return "false"
}

func (s *CLIConfigService) isSymlink(path string) bool {
	if li, err := os.Lstat(path); err == nil {
		return li.Mode()&os.ModeSymlink != 0
	}
	return false
}

func (s *CLIConfigService) resolveSymlink(path string) (string, error) {
	if !s.isSymlink(path) {
		return path, nil
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path, fmt.Errorf("failed to resolve symlink: %w", err)
	}
	logger.Info("Resolved symlink: %s -> %s", path, realPath)
	return realPath, nil
}

func (s *CLIConfigService) validateConfigPath(cliType CLIType, filePath string) error {
	if s.isSymlink(filePath) {
		logger.Warn("%s config is a symbolic link: %s", cliType, filePath)
		resolved, err := s.resolveSymlink(filePath)
		if err != nil {
			return fmt.Errorf("invalid symlink: %w", err)
		}
		if !s.isDirWritable(filepath.Dir(resolved)) {
			return fmt.Errorf("symlink target directory not writable: %s", filepath.Dir(resolved))
		}
	}
	return nil
}

func (s *CLIConfigService) getClaudeSettingsPath() (string, error) {
	homeDir, err := s.getHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".claude", "settings.json"), nil
}

func (s *CLIConfigService) getCodexConfigPath() (string, error) {
	homeDir, err := s.getHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome != "" {
		return filepath.Join(codexHome, "config.toml"), nil
	}
	return filepath.Join(homeDir, ".codex", "config.toml"), nil
}

func (s *CLIConfigService) getGeminiSettingsPath() (string, error) {
	if s.isContainer {
		if path := os.Getenv("GEMINI_CONFIG_PATH"); path != "" {
			return path, nil
		}
	}
	homeDir, err := s.getHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "gemini-cli", "config.json"), nil
		}
		return filepath.Join(homeDir, ".gemini", "settings.json"), nil
	case "darwin":
		return filepath.Join(homeDir, "Library", "Application Support", "gemini-cli", "config.json"), nil
	default:
		xdgConfig := os.Getenv("XDG_CONFIG_HOME")
		if xdgConfig != "" {
			geminiPath := filepath.Join(xdgConfig, "gemini", "settings.json")
			if _, err := os.Stat(geminiPath); err == nil {
				return geminiPath, nil
			}
		}
		newPath := filepath.Join(homeDir, ".gemini", "settings.json")
		if _, err := os.Stat(newPath); err == nil {
			return newPath, nil
		}
		oldPath := filepath.Join(homeDir, ".config", "gemini-cli", "config.json")
		if _, err := os.Stat(oldPath); err == nil {
			return oldPath, nil
		}
		return newPath, nil
	}
}

func getContainerProxyHost() string {
	if hostEnv := os.Getenv("CC_NEXUS_PROXY_HOST"); hostEnv != "" {
		return hostEnv
	}
	return "host.docker.internal"
}

func (s *CLIConfigService) isDirWritable(dir string) bool {
	if !s.isContainer {
		return true
	}
	testFile := filepath.Join(dir, ".ccg_write_test")
	err := os.WriteFile(testFile, []byte("test"), 0644)
	if err != nil {
		return false
	}
	os.Remove(testFile)
	return true
}

func (s *CLIConfigService) validateContainerConfig(cliType CLIType) error {
	if !s.isContainer {
		return nil
	}

	var configDir string
	switch cliType {
	case CLITypeClaude:
		homeDir, err := s.getHomeDir()
		if err != nil {
			return fmt.Errorf("container: failed to get home directory: %w", err)
		}
		configDir = filepath.Join(homeDir, ".claude")
	case CLITypeCodex:
		homeDir, err := s.getHomeDir()
		if err != nil {
			return fmt.Errorf("container: failed to get home directory: %w", err)
		}
		configDir = filepath.Join(homeDir, ".codex")
	case CLITypeGemini:
		geminiPath, err := s.getGeminiSettingsPath()
		if err != nil {
			return fmt.Errorf("container: failed to get Gemini config path: %w", err)
		}
		configDir = filepath.Dir(geminiPath)
	}

	if !s.isDirWritable(configDir) {
		return fmt.Errorf("container: directory not writable: %s. Please mount this directory as a volume", configDir)
	}
	return nil
}

func (s *CLIConfigService) getProxyBaseURL() string {
	port := s.config.GetPort()
	host := "localhost"
	if s.isContainer {
		host = getContainerProxyHost()
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

func (s *CLIConfigService) backupConfig(cliType CLIType) error {
	s.backupsMutex.Lock()
	defer s.backupsMutex.Unlock()

	if _, exists := s.backups[cliType]; exists {
		return nil
	}

	var filePath string
	var err error

	switch cliType {
	case CLITypeClaude:
		filePath, err = s.getClaudeSettingsPath()
	case CLITypeCodex:
		filePath, err = s.getCodexConfigPath()
	case CLITypeGemini:
		filePath, err = s.getGeminiSettingsPath()
	default:
		return fmt.Errorf("unsupported CLI type: %s", cliType)
	}

	if err != nil {
		return err
	}

	backup := CLIConfigBackup{
		FilePath: filePath,
		Exists:   false,
	}

	if _, err := os.Stat(filePath); err == nil {
		content, readErr := os.ReadFile(filePath)
		if readErr != nil {
			return fmt.Errorf("failed to read config file: %w", readErr)
		}
		backup.OriginalContent = string(content)
		backup.Exists = true
		logger.Debug("Backed up %s config from: %s", cliType, filePath)

		if s.isContainer {
			backupFile := filePath + ".ccg.original"
			if writeErr := os.WriteFile(backupFile, content, 0644); writeErr != nil {
				logger.Warn("Failed to create persistent backup %s: %v", backupFile, writeErr)
			} else {
				logger.Info("Created persistent backup for container recovery: %s", backupFile)
			}
		}
	} else if os.IsNotExist(err) {
		logger.Debug("Config file does not exist, will create new: %s", filePath)
	} else {
		return fmt.Errorf("failed to stat config file: %w", err)
	}

	s.backups[cliType] = backup
	return nil
}

func (s *CLIConfigService) restoreConfig(cliType CLIType) error {
	s.backupsMutex.Lock()
	backup, exists := s.backups[cliType]
	s.backupsMutex.Unlock()

	if !exists {
		return nil
	}

	if !backup.Exists {
		if _, err := os.Stat(backup.FilePath); err == nil {
			if err := os.Remove(backup.FilePath); err != nil {
				return fmt.Errorf("failed to remove created config file: %w", err)
			}
			logger.Info("Removed created %s config file: %s", cliType, backup.FilePath)
		}
		return nil
	}

	dir := filepath.Dir(backup.FilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	if err := os.WriteFile(backup.FilePath, []byte(backup.OriginalContent), 0644); err != nil {
		return fmt.Errorf("failed to restore config file: %w", err)
	}

	logger.Info("Restored %s config from backup: %s", cliType, backup.FilePath)

	if s.isContainer {
		persistentBackup := backup.FilePath + ".ccg.original"
		if _, err := os.Stat(persistentBackup); err == nil {
			if err := os.Remove(persistentBackup); err != nil {
				logger.Warn("Failed to remove persistent backup file: %v", err)
			} else {
				logger.Info("Cleaned up persistent backup: %s", persistentBackup)
			}
		}
	}

	return nil
}

func (s *CLIConfigService) RestoreAllConfigs() {
	for _, cliType := range []CLIType{CLITypeClaude, CLITypeCodex, CLITypeGemini} {
		if err := s.restoreConfig(cliType); err != nil {
			logger.Error("Failed to restore %s config: %v", cliType, err)
		}
	}
}

func (s *CLIConfigService) RestoreCLIConfig(cliType CLIType) error {
	return s.restoreConfig(cliType)
}

type ClaudeSettingsForCLI struct {
	Env   map[string]string `json:"env"`
	Model string            `json:"model,omitempty"`
}

func (s *CLIConfigService) readClaudeSettings() (ClaudeSettingsForCLI, error) {
	path, err := s.getClaudeSettingsPath()
	if err != nil {
		return ClaudeSettingsForCLI{}, err
	}

	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return ClaudeSettingsForCLI{Env: make(map[string]string)}, nil
	}

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return ClaudeSettingsForCLI{}, fmt.Errorf("failed to read file: %w", readErr)
	}

	var settings ClaudeSettingsForCLI
	if unmarshalErr := json.Unmarshal(data, &settings); unmarshalErr != nil {
		logger.Warn("Failed to parse Claude settings.json, creating new: %v", unmarshalErr)
		return ClaudeSettingsForCLI{Env: make(map[string]string)}, nil
	}

	if settings.Env == nil {
		settings.Env = make(map[string]string)
	}

	return settings, nil
}

func (s *CLIConfigService) writeClaudeSettings(settings ClaudeSettingsForCLI) error {
	path, err := s.getClaudeSettingsPath()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	data, marshalErr := json.MarshalIndent(settings, "", "  ")
	if marshalErr != nil {
		return fmt.Errorf("failed to marshal JSON: %w", marshalErr)
	}

	if writeErr := os.WriteFile(path, data, 0644); writeErr != nil {
		return fmt.Errorf("failed to write file: %w", writeErr)
	}

	return nil
}

func (s *CLIConfigService) ApplyClaudeConfig() error {
	if err := s.validateContainerConfig(CLITypeClaude); err != nil {
		return err
	}
	if err := s.backupConfig(CLITypeClaude); err != nil {
		return fmt.Errorf("failed to backup Claude config: %w", err)
	}

	settings, err := s.readClaudeSettings()
	if err != nil {
		return fmt.Errorf("failed to read Claude settings: %w", err)
	}

	baseURL := s.getProxyBaseURL()
	settings.Env["ANTHROPIC_API_KEY"] = ""
	settings.Env["ANTHROPIC_AUTH_TOKEN"] = "anything"
	settings.Env["ANTHROPIC_BASE_URL"] = baseURL
	if settings.Model == "" {
		settings.Model = "claude-sonnet-4-6"
	}

	if err := s.writeClaudeSettings(settings); err != nil {
		return fmt.Errorf("failed to write Claude settings: %w", err)
	}

	s.modifiedMu.Lock()
	s.modified[CLITypeClaude] = true
	s.modifiedMu.Unlock()

	logger.Info("Applied Claude config with base URL: %s, model: %s (API_KEY cleared, AUTH_TOKEN set for Bearer auth)", baseURL, settings.Model)
	return nil
}

func (s *CLIConfigService) ApplyCodexConfig(providerName string) error {
	if err := s.validateContainerConfig(CLITypeCodex); err != nil {
		return err
	}
	if err := s.backupConfig(CLITypeCodex); err != nil {
		return fmt.Errorf("failed to backup Codex config: %w", err)
	}

	filePath, err := s.getCodexConfigPath()
	if err != nil {
		return err
	}

	baseURL := s.getProxyBaseURL()
	if providerName == "" {
		providerName = "ccg"
	}

	newProviderConfig := `[model_providers.` + providerName + `]
name = "` + providerName + `"
base_url = "` + baseURL + `/v1"
wire_api = "responses"
requires_openai_auth = true
`

	newContent, err := s.mergeCodexProviderConfig(filePath, providerName, newProviderConfig)
	if err != nil {
		return fmt.Errorf("failed to merge Codex config: %w", err)
	}

	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	if err := os.WriteFile(filePath, []byte(newContent), 0644); err != nil {
		return fmt.Errorf("failed to write Codex config: %w", err)
	}

	s.modifiedMu.Lock()
	s.modified[CLITypeCodex] = true
	s.modifiedMu.Unlock()

	logger.Info("Applied/updated [model_providers.%s] in Codex config with base URL: %s/v1", providerName, baseURL)
	return nil
}

func (s *CLIConfigService) mergeCodexProviderConfig(filePath, providerName, newProviderConfig string) (string, error) {
	providerSection := "[model_providers." + providerName + "]"
	existingContent, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return s.generateCodexConfig(providerName), nil
		}
		return "", err
	}

	content := string(existingContent)
	lines := strings.Split(content, "\n")

	var result []string
	inSection := false
	sectionStart := -1
	sectionEnd := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if inSection && sectionStart >= 0 {
				sectionEnd = i
				break
			}
			if trimmed == providerSection {
				inSection = true
				sectionStart = i
			}
		}
	}

	if inSection && sectionStart >= 0 {
		if sectionEnd < 0 {
			sectionEnd = len(lines)
		}
		result = append(result, lines[:sectionStart]...)
		result = append(result, strings.Split(strings.TrimSuffix(newProviderConfig, "\n"), "\n")...)
		if sectionEnd < len(lines) {
			result = append(result, lines[sectionEnd:]...)
		}
	} else {
		result = lines
		if len(result) > 0 && strings.TrimSpace(result[len(result)-1]) != "" {
			result = append(result, "")
		}
		result = append(result, strings.Split(strings.TrimPrefix(newProviderConfig, "\n"), "\n")...)
	}

	return strings.Join(result, "\n"), nil
}

func (s *CLIConfigService) generateCodexConfig(providerName string) string {
	baseURL := s.getProxyBaseURL()
	var sb strings.Builder
	sb.WriteString("# Codex CLI Configuration\n")
	sb.WriteString("# Generated by ccg on " + runtime.GOOS + "\n")
	sb.WriteString("# Documentation: https://github.com/openai/codex/blob/main/docs/config.md\n\n")
	sb.WriteString("model = \"gpt-5.4\"\n")
	sb.WriteString("model_provider = \"" + providerName + "\"\n")
	sb.WriteString("preferred_auth_method = \"apikey\"\n")
	sb.WriteString("approval_policy = \"never\"\n")
	sb.WriteString("review_model = \"gpt-5.4\"\n")
	sb.WriteString("model_reasoning_summary = \"detailed\"\n")
	sb.WriteString("model_verbosity = \"high\"\n")
	sb.WriteString("model_reasoning_effort = \"high\"\n")
	sb.WriteString("model_context_window = 1050000\n")
	sb.WriteString("model_auto_compact_token_limit = 900000\n")
	sb.WriteString("tool_output_token_limit = 100000\n\n")
	sb.WriteString("[model_providers." + providerName + "]\n")
	sb.WriteString("name = \"" + providerName + "\"\n")
	sb.WriteString("base_url = \"" + baseURL + "/v1\"\n")
	sb.WriteString("wire_api = \"responses\"\n")
	sb.WriteString("requires_openai_auth = true\n")
	return sb.String()
}

type GeminiSettings struct {
	General   map[string]any      `json:"general,omitempty"`
	Model     GeminiModelSettings `json:"model,omitempty"`
	API       GeminiAPISettings   `json:"api,omitempty"`
	Auth      GeminiAuthSettings  `json:"auth,omitempty"`
	Tools     map[string]any      `json:"tools,omitempty"`
	Context   map[string]any      `json:"context,omitempty"`
	Telemetry map[string]any      `json:"telemetry,omitempty"`
}

type GeminiModelSettings struct {
	Name string `json:"name,omitempty"`
}

type GeminiAPISettings struct {
	Endpoint string `json:"endpoint,omitempty"`
}

type GeminiAuthSettings struct {
	Method string `json:"method,omitempty"`
	APIKey string `json:"apiKey,omitempty"`
}

func (s *CLIConfigService) ApplyGeminiConfig(apiKey string) error {
	if err := s.validateContainerConfig(CLITypeGemini); err != nil {
		return err
	}
	if err := s.backupConfig(CLITypeGemini); err != nil {
		return fmt.Errorf("failed to backup Gemini config: %w", err)
	}

	filePath, err := s.getGeminiSettingsPath()
	if err != nil {
		return err
	}

	var settings GeminiSettings
	if _, statErr := os.Stat(filePath); statErr == nil {
		data, readErr := os.ReadFile(filePath)
		if readErr != nil {
			return fmt.Errorf("failed to read Gemini config: %w", readErr)
		}
		if unmarshalErr := json.Unmarshal(data, &settings); unmarshalErr != nil {
			logger.Warn("Failed to parse Gemini config, creating new: %v", unmarshalErr)
			settings = GeminiSettings{}
		}
	}

	baseURL := s.getProxyBaseURL() + "/v1beta"
	if settings.API.Endpoint == "" {
		settings.API = GeminiAPISettings{}
	}
	settings.API.Endpoint = baseURL
	if settings.Model.Name == "" {
		settings.Model = GeminiModelSettings{
			Name: "gemini-3.1-pro",
		}
	}
	if settings.Auth.Method == "" {
		settings.Auth = GeminiAuthSettings{
			Method: "api-key",
		}
	}
	if apiKey != "" {
		settings.Auth.APIKey = apiKey
	} else if settings.Auth.APIKey == "" {
		settings.Auth.APIKey = "$OPENAI_API_KEY"
	}

	dir := filepath.Dir(filePath)
	if mkdirErr := os.MkdirAll(dir, 0755); mkdirErr != nil {
		return fmt.Errorf("failed to create directory: %w", mkdirErr)
	}

	output, marshalErr := json.MarshalIndent(settings, "", "  ")
	if marshalErr != nil {
		return fmt.Errorf("failed to marshal JSON: %w", marshalErr)
	}

	if writeErr := os.WriteFile(filePath, output, 0644); writeErr != nil {
		return fmt.Errorf("failed to write Gemini config: %w", writeErr)
	}

	s.modifiedMu.Lock()
	s.modified[CLITypeGemini] = true
	s.modifiedMu.Unlock()

	logger.Info("Applied Gemini config with endpoint: %s (auth method: api-key, apiKey: $OPENAI_API_KEY env var)", baseURL)
	return nil
}

func (s *CLIConfigService) IsModified(cliType CLIType) bool {
	s.modifiedMu.RLock()
	defer s.modifiedMu.RUnlock()
	return s.modified[cliType]
}

func (s *CLIConfigService) IsContainer() bool {
	return s.isContainer
}

func (s *CLIConfigService) GetConfigPaths() map[string]string {
	paths := make(map[string]string)
	paths["claude"], _ = s.getClaudeSettingsPath()
	paths["codex"], _ = s.getCodexConfigPath()
	paths["gemini"], _ = s.getGeminiSettingsPath()
	return paths
}
