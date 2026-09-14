package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	aigcrepository "github.com/QuantumNous/new-api/aigc/repository"
	aigcservice "github.com/QuantumNous/new-api/aigc/service"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/joho/godotenv"
	"github.com/shopspring/decimal"
)

const maxRequestBodyBytes = 64 << 10

type config struct {
	ListenAddress   string
	ServerCertFile  string
	ServerKeyFile   string
	ClientCAFile    string
	AllowedClientID string
	ShutdownTimeout time.Duration
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type envelope struct {
	Success bool      `json:"success"`
	Data    any       `json:"data,omitempty"`
	Error   *apiError `json:"error,omitempty"`
}

type identityPayload struct {
	EventID          int64  `json:"event_id"`
	IAMUserID        int64  `json:"iam_user_id"`
	OrganizationID   int64  `json:"organization_id"`
	OrganizationType int16  `json:"organization_type"`
	DisplayName      string `json:"display_name"`
	DesiredState     string `json:"desired_state"`
	LifecycleVersion int64  `json:"lifecycle_version"`
	SourceCreatedAt  int64  `json:"source_created_at"`
}

type createAPIKeyPayload struct {
	EventID            int64    `json:"event_id"`
	APIKeyID           int64    `json:"api_key_id"`
	APIKeyVersion      int64    `json:"api_key_version"`
	IAMUserID          int64    `json:"iam_user_id"`
	Name               string   `json:"name"`
	ExpiresAt          int64    `json:"expires_at,omitempty"`
	ModelLimits        []string `json:"model_limits,omitempty"`
	AllowedIPCIDRs     []string `json:"allowed_ip_cidrs,omitempty"`
	RequestFingerprint string   `json:"request_fingerprint"`
}

type revokeAPIKeyPayload struct {
	EventID       int64 `json:"event_id"`
	IAMUserID     int64 `json:"iam_user_id"`
	APIKeyID      int64 `json:"api_key_id"`
	APIKeyVersion int64 `json:"api_key_version"`
}

type pricingCatalog interface {
	List(ctx context.Context, group string) (aigcservice.PublicPricingDocument, error)
}

type pricingRouteDependencies struct {
	resolveActiveIAMIdentity func(iamUserID, minimumVersion int64) (model.IAMIdentityLink, error)
	getUserGroup             func(userID int, fromDB bool) (string, error)
	catalog                  pricingCatalog
	usdExchangeRate          func() float64
}

type controlPricingDocument struct {
	aigcservice.PublicPricingDocument
	USDToCNYRate string `json:"usd_to_cny_rate"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	_ = godotenv.Load(".env")
	common.InitEnv()
	logger.SetupLogger()
	ratio_setting.InitRatioSettings()
	if common.IsMasterNode {
		return errors.New("new-api-control must run with NODE_TYPE=slave so it never owns schema migration")
	}
	if err := model.InitDB(); err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}
	if err := model.InitLogDB(); err != nil {
		return fmt.Errorf("initialize log database: %w", err)
	}
	defer model.CloseDB()
	model.InitOptionMap()
	optionSyncContext, stopOptionSync := context.WithCancel(ctx)
	optionSyncDone := startControlOptionSync(optionSyncContext, model.SyncOptionsContext)
	defer func() {
		stopOptionSync()
		<-optionSyncDone
	}()
	if err := common.InitRedisClient(); err != nil {
		return fmt.Errorf("initialize Redis: %w", err)
	}
	processConfig, err := loadConfig()
	if err != nil {
		return err
	}
	tlsConfig, err := loadMTLSConfig(processConfig)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              processConfig.ListenAddress,
		Handler:           routes(),
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	errChannel := make(chan error, 1)
	go func() {
		errChannel <- server.ListenAndServeTLS(processConfig.ServerCertFile, processConfig.ServerKeyFile)
	}()
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), processConfig.ShutdownTimeout)
		defer cancel()
		return server.Shutdown(shutdownContext)
	case err := <-errChannel:
		return err
	}
}

func startControlOptionSync(ctx context.Context, syncOptions func(context.Context, int)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		syncOptions(ctx, common.SyncFrequency)
	}()
	return done
}

func loadConfig() (config, error) {
	result := config{
		ListenAddress:   strings.TrimSpace(os.Getenv("NEW_API_CONTROL_LISTEN_ADDRESS")),
		ServerCertFile:  strings.TrimSpace(os.Getenv("NEW_API_CONTROL_TLS_CERT_FILE")),
		ServerKeyFile:   strings.TrimSpace(os.Getenv("NEW_API_CONTROL_TLS_KEY_FILE")),
		ClientCAFile:    strings.TrimSpace(os.Getenv("NEW_API_CONTROL_CLIENT_CA_FILE")),
		AllowedClientID: strings.TrimSpace(os.Getenv("NEW_API_CONTROL_ALLOWED_CLIENT_ID")),
		ShutdownTimeout: 10 * time.Second,
	}
	if result.ListenAddress == "" {
		result.ListenAddress = ":3010"
	}
	if result.ServerCertFile == "" || result.ServerKeyFile == "" || result.ClientCAFile == "" || result.AllowedClientID == "" {
		return config{}, errors.New("new-api-control TLS cert, key, dedicated client CA, and allowed client identity are required")
	}
	return result, nil
}

func loadMTLSConfig(processConfig config) (*tls.Config, error) {
	caPEM, err := os.ReadFile(processConfig.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read client CA: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("client CA file contains no valid certificate")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  clientCAs,
		NextProtos: []string{"h2", "http/1.1"},
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
				return errors.New("client certificate was not verified")
			}
			certificate := state.VerifiedChains[0][0]
			if certificate.Subject.CommonName == processConfig.AllowedClientID {
				return nil
			}
			for _, name := range certificate.DNSNames {
				if name == processConfig.AllowedClientID {
					return nil
				}
			}
			for _, uri := range certificate.URIs {
				if uri.String() == processConfig.AllowedClientID {
					return nil
				}
			}
			return errors.New("client certificate identity is not allowed")
		},
	}, nil
}

func routes() http.Handler {
	profiles := aigcrepository.New(model.DB)
	pricing := aigcservice.NewPricingService(
		profiles,
		aigcservice.NewModelAvailability(),
		aigcservice.NewModelUpstreamSource(),
	)
	return routesWithPricingDependencies(pricingRouteDependencies{
		resolveActiveIAMIdentity: model.ResolveActiveIAMIdentity,
		getUserGroup:             model.GetUserGroup,
		catalog:                  pricing,
		usdExchangeRate:          func() float64 { return operation_setting.USDExchangeRate },
	})
}

func routesWithPricingDependencies(dependencies pricingRouteDependencies) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, envelope{Success: true, Data: map[string]string{"status": "live"}})
	})
	mux.HandleFunc("GET /health/ready", handleReady)
	mux.HandleFunc("POST /internal/v1/identities/apply", handleApplyIdentity)
	mux.HandleFunc("POST /internal/v1/api-keys/create", handleCreateAPIKey)
	mux.HandleFunc("POST /internal/v1/api-keys/authenticate", handleAuthenticateAPIKey)
	mux.HandleFunc("POST /internal/v1/api-keys/revoke", handleRevokeAPIKey)
	mux.HandleFunc("GET /internal/v1/api-keys/{apiKeyID}", handleGetAPIKey)
	mux.HandleFunc("GET /internal/v1/chat-models", handleListChatModels)
	mux.HandleFunc("GET /internal/v1/aigc/pricing", handleListAIGCPricing(dependencies))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBodyBytes)
		next.ServeHTTP(writer, request)
	})
}

func handleReady(writer http.ResponseWriter, request *http.Request) {
	sqlDB, err := model.DB.DB()
	if err == nil {
		ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
		defer cancel()
		err = sqlDB.PingContext(ctx)
	}
	if err != nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "NOT_READY", "database is unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, envelope{Success: true, Data: map[string]string{"status": "ready"}})
}

func handleApplyIdentity(writer http.ResponseWriter, request *http.Request) {
	var payload identityPayload
	if !decodeRequest(writer, request, &payload) {
		return
	}
	result, err := model.ApplyIAMIdentity(model.ApplyIAMIdentityInput{
		EventID: payload.EventID, IAMUserID: payload.IAMUserID, OrganizationID: payload.OrganizationID,
		OrganizationType: payload.OrganizationType, DisplayName: payload.DisplayName,
		DesiredState: payload.DesiredState, LifecycleVersion: payload.LifecycleVersion,
		SourceCreatedAt: payload.SourceCreatedAt,
	})
	if err != nil {
		writeModelError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, envelope{Success: true, Data: result})
}

func handleCreateAPIKey(writer http.ResponseWriter, request *http.Request) {
	var payload createAPIKeyPayload
	if !decodeRequest(writer, request, &payload) {
		return
	}
	if len(payload.ModelLimits) > 256 || len(payload.AllowedIPCIDRs) > 64 {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "too many model or IP restrictions")
		return
	}
	for _, rawPrefix := range payload.AllowedIPCIDRs {
		if _, err := netip.ParsePrefix(strings.TrimSpace(rawPrefix)); err != nil {
			writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "allowed_ip_cidrs contains an invalid CIDR")
			return
		}
	}
	if payload.ExpiresAt > 0 && payload.ExpiresAt <= time.Now().Unix() {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "expires_at must be in the future")
		return
	}
	result, err := model.CreateIAMAPIKey(model.CreateIAMAPIKeyInput{
		EventID: payload.EventID, IAMAPIKeyID: payload.APIKeyID, IAMUserID: payload.IAMUserID,
		Version: payload.APIKeyVersion, Name: payload.Name, ExpiresAt: payload.ExpiresAt,
		ModelLimits: payload.ModelLimits, AllowedIPCIDRs: payload.AllowedIPCIDRs,
		RequestFingerprint: payload.RequestFingerprint,
	})
	if err != nil {
		writeModelError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, envelope{Success: true, Data: result})
}

func handleRevokeAPIKey(writer http.ResponseWriter, request *http.Request) {
	var payload revokeAPIKeyPayload
	if !decodeRequest(writer, request, &payload) {
		return
	}
	result, err := model.RevokeIAMAPIKey(payload.EventID, payload.IAMUserID, payload.APIKeyID, payload.APIKeyVersion)
	if err != nil {
		writeModelError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, envelope{Success: true, Data: result})
}

func handleGetAPIKey(writer http.ResponseWriter, request *http.Request) {
	apiKeyID, err := strconv.ParseInt(request.PathValue("apiKeyID"), 10, 64)
	if err != nil || apiKeyID <= 0 {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "api key ID is invalid")
		return
	}
	iamUserID, err := strconv.ParseInt(request.URL.Query().Get("iam_user_id"), 10, 64)
	if err != nil || iamUserID <= 0 {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "IAM user ID is invalid")
		return
	}
	result, err := model.GetIAMAPIKeyState(iamUserID, apiKeyID)
	if err != nil {
		writeModelError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, envelope{Success: true, Data: result})
}

func handleListChatModels(writer http.ResponseWriter, request *http.Request) {
	iamUserID, err := strconv.ParseInt(request.URL.Query().Get("iam_user_id"), 10, 64)
	if err != nil || iamUserID <= 0 {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "IAM user ID is invalid")
		return
	}
	identityVersion, err := strconv.ParseInt(request.URL.Query().Get("identity_version"), 10, 64)
	if err != nil || identityVersion <= 0 {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "identity version is invalid")
		return
	}
	identity, err := model.ResolveActiveIAMIdentity(iamUserID, identityVersion)
	if err != nil {
		writeModelError(writer, err)
		return
	}
	models, err := controller.GetUserChatModelsForUser(identity.NewAPIUserID)
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "UPSTREAM_INTERNAL", "chat model catalog is unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, envelope{Success: true, Data: models})
}

func handleListAIGCPricing(dependencies pricingRouteDependencies) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		for _, name := range []string{"Authorization", "Cookie", "New-Api-User", "Session"} {
			if strings.TrimSpace(request.Header.Get(name)) != "" {
				writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "identity headers are not accepted")
				return
			}
		}
		iamUserID, err := strconv.ParseInt(request.URL.Query().Get("iam_user_id"), 10, 64)
		if err != nil || iamUserID <= 0 {
			writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "IAM user ID is invalid")
			return
		}
		identityVersion, err := strconv.ParseInt(request.URL.Query().Get("identity_version"), 10, 64)
		if err != nil || identityVersion <= 0 {
			writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "identity version is invalid")
			return
		}
		identity, err := dependencies.resolveActiveIAMIdentity(iamUserID, identityVersion)
		if err != nil {
			writeModelError(writer, err)
			return
		}
		group, err := dependencies.getUserGroup(identity.NewAPIUserID, true)
		if err != nil {
			common.SysError("new-api-control failed to load pricing group: " + err.Error())
			writeAPIError(writer, http.StatusInternalServerError, "PRICING_UNAVAILABLE", "AIGC pricing is unavailable")
			return
		}
		group = strings.TrimSpace(group)
		if strings.EqualFold(group, "auto") {
			writeAPIError(writer, http.StatusConflict, "AUTO_GROUP_UNSUPPORTED", "AIGC pricing is unavailable for auto group")
			return
		}
		if group == "" {
			writeAPIError(writer, http.StatusInternalServerError, "PRICING_UNAVAILABLE", "AIGC pricing group is unavailable")
			return
		}
		document, err := dependencies.catalog.List(request.Context(), group)
		if err != nil {
			common.SysError("new-api-control failed to load AIGC pricing: " + err.Error())
			writeAPIError(writer, http.StatusInternalServerError, "PRICING_UNAVAILABLE", "AIGC pricing is unavailable")
			return
		}
		exchangeRate := dependencies.usdExchangeRate()
		if exchangeRate <= 0 || math.IsNaN(exchangeRate) || math.IsInf(exchangeRate, 0) {
			writeAPIError(writer, http.StatusInternalServerError, "PRICING_UNAVAILABLE", "USD exchange rate is unavailable")
			return
		}
		writeJSON(writer, http.StatusOK, envelope{Success: true, Data: controlPricingDocument{
			PublicPricingDocument: document,
			USDToCNYRate:          decimal.NewFromFloat(exchangeRate).String(),
		}})
	}
}

func decodeRequest(writer http.ResponseWriter, request *http.Request, destination any) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeAPIError(writer, http.StatusUnsupportedMediaType, "INVALID_ARGUMENT", "Content-Type must be application/json")
		return false
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "request body cannot be read")
		return false
	}
	if len(body) == 0 || common.Unmarshal(body, destination) != nil {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "request body is invalid JSON")
		return false
	}
	return true
}

func writeModelError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrIAMInvalidInput):
		writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "control request is invalid")
	case errors.Is(err, model.ErrIAMIdentityNotActive):
		writeAPIError(writer, http.StatusConflict, "IDENTITY_NOT_ACTIVE", "IAM identity is not active")
	case errors.Is(err, model.ErrIAMVersionConflict):
		writeAPIError(writer, http.StatusConflict, "VERSION_CONFLICT", "lifecycle version conflicts with existing state")
	case errors.Is(err, model.ErrIAMAPIKeyNotFound):
		writeAPIError(writer, http.StatusNotFound, "API_KEY_NOT_FOUND", "API key does not exist")
	default:
		common.SysError("new-api-control request failed: " + err.Error())
		writeAPIError(writer, http.StatusInternalServerError, "INTERNAL", "control operation failed")
	}
}

func writeAPIError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, envelope{Success: false, Error: &apiError{Code: code, Message: message}})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	payload, err := common.Marshal(value)
	if err != nil {
		http.Error(writer, "internal encoding error", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}
