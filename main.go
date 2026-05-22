package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"github.com/google/uuid"
	"github.com/jhillyerd/enmime"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/yaml.v3"
)

//go:embed index.html
var indexHTMLTemplate string

type SenderRule struct {
	Match    string         `yaml:"match"`
	IsRegexp bool           `yaml:"is_regexp"`
	Compiled *regexp.Regexp `yaml:"-"`
}

type Credential struct {
	Username       string       `yaml:"username"`
	Password       string       `yaml:"password"`
	AllowedSenders []SenderRule `yaml:"allowed_senders"`
}

type StorageBackendConfig struct {
	Name        string            `yaml:"name"`
	Type        string            `yaml:"type"` 
	UploadURL   string            `yaml:"upload_url"`
	FileField   string            `yaml:"file_field"`
	ExtraFields map[string]string `yaml:"extra_fields"`
}

type StrategyConfig struct {
	BodyMode           string `yaml:"body_mode"`            
	AttachmentStorage  string `yaml:"attachment_storage"`   
	InlineImageStorage string `yaml:"inline_image_storage"` 
}

type WebhookConfig struct {
	URL                string            `yaml:"url"`
	Method             string            `yaml:"method"`
	Headers            map[string]string `yaml:"headers"`
	BodyTemplate       string            `yaml:"body_template"`
	ProcessingStrategy StrategyConfig    `yaml:"processing_strategy"`
}

type RouteConfig struct {
	FromMatch    string         `yaml:"from_match"`
	FromIsRegexp bool           `yaml:"from_is_regexp"`
	FromCompiled *regexp.Regexp `yaml:"-"`
	ToMatch      string         `yaml:"to_match"`
	ToIsRegexp   bool           `yaml:"to_is_regexp"`
	ToCompiled *regexp.Regexp `yaml:"-"`
	Webhook      WebhookConfig  `yaml:"webhook"`
}

type Config struct {
	Server struct {
		Addr            string `yaml:"addr"`
		Domain          string `yaml:"domain"`
		MaxMessageBytes int64  `yaml:"max_message_bytes"`
		AuthEnabled     bool   `yaml:"auth_enabled"`
		LogLevel        string `yaml:"log_level"`
		WebAddr         string `yaml:"web_addr"` 
	} `yaml:"server"`
	Credentials    []Credential           `yaml:"credentials"`
	Storages       []StorageBackendConfig `yaml:"storages"`
	DefaultWebhook WebhookConfig          `yaml:"default_webhook"`
	Routes         []RouteConfig          `yaml:"routes"`
}

type PageData struct {
	Config  Config
	Message string
}

var (
	globalConfig   Config
	configLock     sync.RWMutex
	logger         *zap.Logger     
	zapAtomicLevel zap.AtomicLevel 
	configPath     string          
)

func initLoggerAndFlags() {
	flag.StringVar(&configPath, "config", "config.yaml", "Path to the configuration YAML file")
	flag.Parse()

	zapAtomicLevel = zap.NewAtomicLevelAt(zap.InfoLevel)
	encoderCfg := zap.NewDevelopmentEncoderConfig()
	encoderCfg.EncodeTime = zapcore.TimeEncoderOfLayout("2006-01-02 15:04:05")
	encoderCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder

	core := zapcore.NewCore(zapcore.NewConsoleEncoder(encoderCfg), os.Stdout, zapAtomicLevel)
	logger = zap.New(core, zap.AddCaller())
}

func updateLogLevel(levelStr string) {
	switch strings.ToLower(levelStr) {
	case "debug": zapAtomicLevel.SetLevel(zap.DebugLevel)
	case "info":  zapAtomicLevel.SetLevel(zap.InfoLevel)
	case "warn":  zapAtomicLevel.SetLevel(zap.WarnLevel)
	case "error": zapAtomicLevel.SetLevel(zap.ErrorLevel)
	default:      zapAtomicLevel.SetLevel(zap.InfoLevel)
	}
}

type MediaItem struct {
	FileName    string `json:"filename"`
	ContentType string `json:"content_type"`
	URL         string `json:"url"`
}

func replacePlaceholders(input string, uuidStr, from, to, subject, textPrefer, text, html string) string {
	escapeJSON := func(rawStr string) string {
		b, err := json.Marshal(rawStr)
		if err != nil { return "" }
		res := string(b)
		if len(res) >= 2 { return res[1 : len(res)-1] }
		return res
	}
	output := input
	output = strings.ReplaceAll(output, "{uuid}", escapeJSON(uuidStr))
	output = strings.ReplaceAll(output, "{from}", escapeJSON(from))
	output = strings.ReplaceAll(output, "{to}", escapeJSON(to))
	output = strings.ReplaceAll(output, "{subject}", escapeJSON(subject))
	output = strings.ReplaceAll(output, "{text_prefer}", escapeJSON(textPrefer))
	output = strings.ReplaceAll(output, "{text}", escapeJSON(text))
	output = strings.ReplaceAll(output, "{html}", escapeJSON(html))
	return output
}

func uploadToCustomStorage(storageName string, filename string, contentType string, content []byte, uuidStr string) string {
	configLock.RLock()
	var targetStorage *StorageBackendConfig
	for _, storage := range globalConfig.Storages {
		if storage.Name == storageName { targetStorage = &storage; break }
	}
	configLock.RUnlock()

	if targetStorage == nil { return "" }
	parsedUploadURL := strings.ReplaceAll(targetStorage.UploadURL, "{uuid}", uuidStr)

	if targetStorage.Type == "api_form" {
		bodyBuf := &bytes.Buffer{}
		bodyWriter := multipart.NewWriter(bodyBuf)
		fileField := targetStorage.FileField
		if fileField == "" { fileField = "file" }
		fileWriter, err := bodyWriter.CreateFormFile(fileField, filename)
		if err != nil { return "" }
		_, _ = io.Copy(fileWriter, bytes.NewReader(content))

		for key, val := range targetStorage.ExtraFields {
			_ = bodyWriter.WriteField(key, strings.ReplaceAll(val, "{uuid}", uuidStr))
		}
		_ = bodyWriter.Close()

		req, err := http.NewRequest("POST", parsedUploadURL, bodyBuf)
		if err != nil { return "" }
		req.Header.Set("Content-Type", bodyWriter.FormDataContentType())

		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil { return "" }
		defer resp.Body.Close()

		respBytes, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 { return string(respBytes) }
	}
	if targetStorage.Type == "oss" { return parsedUploadURL + filename }
	return ""
}

func processMailDelivery(from string, to []string, data []byte) {
	envelope, err := enmime.ReadEnvelope(bytes.NewReader(data))
	if err != nil {
		logger.Error("郵件基礎 MIME 結構解包失敗", zap.Error(err))
		return
	}

	mailUUID := uuid.New().String()
	subject := envelope.GetHeader("Subject")
	logger.Info("✦ 攔截入站郵件成功並注入清洗管線", zap.String("trace_id", mailUUID), zap.String("subject", subject))

	textPrefer := envelope.Text
	if textPrefer == "" && envelope.HTML != "" { textPrefer = envelope.HTML }

	for _, rcpt := range to {
		configLock.RLock()
		var targetWebhook WebhookConfig
		matched := false
		for _, route := range globalConfig.Routes {
			if (route.FromMatch == "" || (route.FromIsRegexp && route.FromCompiled != nil && route.FromCompiled.MatchString(from)) || (!route.FromIsRegexp && route.FromMatch == from)) &&
				(route.ToMatch == "" || (route.ToIsRegexp && route.ToCompiled != nil && route.ToCompiled.MatchString(rcpt)) || (!route.ToIsRegexp && route.ToMatch == rcpt)) {
				targetWebhook = route.Webhook
				matched = true
				break
			}
		}
		if !matched { targetWebhook = globalConfig.DefaultWebhook }
		configLock.RUnlock()

		strategy := targetWebhook.ProcessingStrategy
		if strategy.AttachmentStorage != "" && strategy.AttachmentStorage != "ignore" {
			for _, file := range envelope.Attachments {
				go uploadToCustomStorage(strategy.AttachmentStorage, file.FileName, file.ContentType, file.Content, mailUUID)
			}
		}
		if strategy.InlineImageStorage != "" && strategy.InlineImageStorage != "ignore" {
			for _, img := range envelope.Inlines {
				go uploadToCustomStorage(strategy.InlineImageStorage, img.FileName, img.ContentType, img.Content, mailUUID)
			}
		}

		finalBodyStr := replacePlaceholders(targetWebhook.BodyTemplate, mailUUID, from, rcpt, subject, textPrefer, envelope.Text, envelope.HTML)

		go func(cfg WebhookConfig, body string, uuidStr string) {
			method := cfg.Method
			if method == "" { method = "POST" }
			parsedURL := strings.ReplaceAll(cfg.URL, "{uuid}", uuidStr)
			req, err := http.NewRequest(method, parsedURL, strings.NewReader(body))
			if err != nil { return }
			req.Header.Set("Content-Type", "application/json")
			for key, value := range cfg.Headers { req.Header.Set(key, strings.ReplaceAll(value, "{uuid}", uuidStr)) }

			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Do(req)
			if err != nil { return }
			defer resp.Body.Close()
			logger.Info("Webhook 事務流轉完畢", zap.String("trace_id", uuidStr), zap.Int("status_code", resp.StatusCode))
		}(targetWebhook, finalBodyStr, mailUUID)
	}
}

type Backend struct{}

func (bkd *Backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &Session{}, nil
}

type Session struct {
	Username      string
	UserCred      *Credential
	From          string
	To            []string
	Authenticated bool
}

func (s *Session) AuthMechanisms() []string {
	return []string{"PLAIN", "LOGIN"}
}

func (s *Session) Auth(mechanism string) (sasl.Server, error) {
	switch mechanism {
	case "PLAIN":
		return sasl.NewPlainServer(func(identity, username, password string) error {
			return s.executeCredentialsCheck(username, password)
		}), nil
	case "LOGIN":
		return &customLoginServer{s: s}, nil
	default:
		return nil, errors.New("unsupported mechanism")
	}
}

func (s *Session) executeCredentialsCheck(username, password string) error {
	configLock.RLock()
	authEnabled := globalConfig.Server.AuthEnabled
	configLock.RUnlock()

	if !authEnabled {
		s.Authenticated = true
		return nil
	}

	configLock.RLock()
	success := false
	var matchedCred Credential
	for _, cred := range globalConfig.Credentials {
		if cred.Username == username && cred.Password == password {
			success = true
			matchedCred = cred
			break
		}
	}
	configLock.RUnlock()

	if success {
		logger.Info("客戶端局域網明文 SMTP 密碼校驗通過", zap.String("user", username))
		s.Username = username
		s.UserCred = &matchedCred
		s.Authenticated = true
		return nil
	}

	logger.Warn("安全攔截：拒絕非法的 SMTP 帳密接入嘗試", zap.String("user", username))
	return errors.New("Invalid username or password")
}

type customLoginServer struct {
	s        *Session
	username string
	step     int
}

func (server *customLoginServer) Next(response []byte) (challenge []byte, done bool, err error) {
	if server.step == 0 {
		if len(response) > 0 {
			server.username = string(response)
			server.step = 2
			return []byte("Password:"), false, nil
		}
		server.step = 1
		return []byte("Username:"), false, nil
	} else if server.step == 1 {
		server.username = string(response)
		server.step = 2
		return []byte("Password:"), false, nil
	} else if server.step == 2 {
		password := string(response)
		err = server.s.executeCredentialsCheck(server.username, password)
		return nil, true, err
	}
	return nil, true, errors.New("sasl: unexpected state")
}

func (s *Session) Mail(from string, opts *smtp.MailOptions) error {
	configLock.RLock()
	authEnabled := globalConfig.Server.AuthEnabled
	configLock.RUnlock()
	if !authEnabled { s.From = from; return nil }

	if !s.Authenticated {
		return &smtp.SMTPError{Code: 530, Message: "Authentication required"}
	}

	if s.UserCred != nil {
		allowed := false
		for _, rule := range s.UserCred.AllowedSenders {
			if (rule.IsRegexp && rule.Compiled != nil && rule.Compiled.MatchString(from)) || (!rule.IsRegexp && rule.Match == from) {
				allowed = true
				break
			}
		}
		if !allowed {
			logger.Error("防線攔截！帳號試圖越權冒用身份發信", zap.String("user", s.Username), zap.String("mail_from", from))
			return &smtp.SMTPError{Code: 554, Message: "Sender address rejected: not authorized"}
		}
	}

	s.From = from
	return nil
}

func (s *Session) Rcpt(to string, opts *smtp.RcptOptions) error {
	s.To = append(s.To, to)
	return nil
}

func (s *Session) Data(r io.Reader) error {
	buf, err := io.ReadAll(r)
	if err != nil { return err }
	go processMailDelivery(s.From, s.To, buf)
	return nil
}

func (s *Session) Reset()        { s.To = nil }
func (s *Session) Logout() error { return nil }

func handleIndex(w http.ResponseWriter, r *http.Request) {
	configLock.RLock()
	data := PageData{Config: globalConfig}
	configLock.RUnlock()
	data.Message = r.URL.Query().Get("msg")
	tmpl, _ := template.New("index").Parse(indexHTMLTemplate)
	tmpl.Execute(w, data)
}

func handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" { return }
	var newCfg Config
	newCfg.Server.Addr = r.FormValue("server_addr")
	newCfg.Server.Domain = r.FormValue("server_domain")
	maxBytes, _ := strconv.ParseInt(r.FormValue("server_max_bytes"), 10, 64)
	newCfg.Server.MaxMessageBytes = maxBytes
	newCfg.Server.AuthEnabled = r.FormValue("server_auth_enabled") == "true"
	
	configLock.RLock()
	newCfg.Server.LogLevel = globalConfig.Server.LogLevel
	newCfg.Server.WebAddr = globalConfig.Server.WebAddr
	configLock.RUnlock()

	newCfg.DefaultWebhook.URL = r.FormValue("default_url")
	newCfg.DefaultWebhook.Method = r.FormValue("default_method")
	newCfg.DefaultWebhook.BodyTemplate = r.FormValue("default_body")

	if r.FormValue("cred_user") != "" {
		cred := Credential{Username: r.FormValue("cred_user"), Password: r.FormValue("cred_pass")}
		if r.FormValue("cred_allowed_sender") != "" {
			cred.AllowedSenders = []SenderRule{{Match: r.FormValue("cred_allowed_sender"), IsRegexp: true}}
		}
		newCfg.Credentials = append(newCfg.Credentials, cred)
	}

	if r.FormValue("route_webhook_url") != "" {
		route := RouteConfig{
			ToMatch: r.FormValue("route_to"), ToIsRegexp: false,
			Webhook: WebhookConfig{
				URL:          r.FormValue("route_webhook_url"),
				Method:       r.FormValue("route_method"),
				BodyTemplate: r.FormValue("route_body"),
				ProcessingStrategy: StrategyConfig{
					BodyMode: r.FormValue("route_body_mode"), AttachmentStorage: r.FormValue("route_attach_store"), InlineImageStorage: "ignore",
				},
			},
		}
		newCfg.Routes = append(newCfg.Routes, route)
	}

	for i, route := range newCfg.Routes {
		if route.FromMatch != "" { newCfg.Routes[i].FromCompiled = regexp.MustCompile(route.FromMatch) }
	}
	for i, cred := range newCfg.Credentials {
		for j, rule := range cred.AllowedSenders {
			if rule.IsRegexp { newCfg.Credentials[i].AllowedSenders[j].Compiled = regexp.MustCompile(rule.Match) }
		}
	}

	yamlBytes, _ := yaml.Marshal(&newCfg)
	_ = os.WriteFile(configPath, yamlBytes, 0644)

	configLock.Lock()
	globalConfig = newCfg
	configLock.Unlock()

	http.Redirect(w, r, "/?msg="+url.QueryEscape("🎉 策略引擎内存热重载成功！"), http.StatusSeeOther)
}

func loadConfig() {
	file, err := os.ReadFile(configPath)
	if err != nil {
		globalConfig.Server.Addr = "0.0.0.0:2525"
		globalConfig.Server.Domain = "localhost"
		globalConfig.Server.MaxMessageBytes = 20 * 1024 * 1024
		globalConfig.Server.AuthEnabled = true
		globalConfig.Server.LogLevel = "info"
		globalConfig.Server.WebAddr = "0.0.0.0:8080" 
		return
	}
	_ = yaml.Unmarshal(file, &globalConfig)
	updateLogLevel(globalConfig.Server.LogLevel)
}

func main() {
	initLoggerAndFlags()
	defer logger.Sync()

	loadConfig()

	configLock.RLock()
	webAddr := globalConfig.Server.WebAddr
	smtpAddr := globalConfig.Server.Addr
	smtpDomain := globalConfig.Server.Domain
	smtpMaxBytes := globalConfig.Server.MaxMessageBytes
	configLock.RUnlock()

	if webAddr == "" { webAddr = "0.0.0.0:8080" }

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/save", handleSave)
	go func() { _ = http.ListenAndServe(webAddr, nil) }()

	be := &Backend{}
	s := smtp.NewServer(be)
	s.Addr = smtpAddr
	s.Domain = smtpDomain
	s.ReadTimeout = 15 * time.Second
	s.WriteTimeout = 15 * time.Second
	s.MaxMessageBytes = smtpMaxBytes

	// 允許在未加密（非 TLS）的連接上進行 AUTH 認證
	s.AllowInsecureAuth = true 

	logger.Info("🚀 網關已就緒", zap.String("smtp_listen", s.Addr))
	if err := s.ListenAndServe(); err != nil {
		logger.Fatal("SMTP 核心網关崩潰", zap.Error(err))
	}
}