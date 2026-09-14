package model

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	ErrIAMAPIKeyUnauthorized = errors.New("invalid or expired API key")
	ErrIAMAPIKeyAccessDenied = errors.New("API key access denied")
)

// IAMAPIKeyPrincipal contains no credential material. IAM must additionally check
// its authoritative identity/key state before using this synchronized projection.
type IAMAPIKeyPrincipal struct {
	APIKeyID          int64    `json:"api_key_id"`
	IAMUserID         int64    `json:"iam_user_id"`
	APIPlatformUserID int64    `json:"api_platform_user_id"`
	OrganizationID    int64    `json:"organization_id"`
	OrganizationType  int16    `json:"organization_type"`
	IdentityVersion   int64    `json:"identity_version"`
	APIKeyVersion     int64    `json:"api_key_version"`
	ModelLimits       []string `json:"model_limits"`
}

// AuthenticateIAMAPIKey is only exposed by the private mTLS control server. It
// reads current database state, bypassing relay caches, and does not consume quota
// or modify a token. Never use a client-supplied identity to resolve this key.
func AuthenticateIAMAPIKey(ctx context.Context, secret, clientIP string) (IAMAPIKeyPrincipal, error) {
	var result IAMAPIKeyPrincipal
	if !strings.HasPrefix(secret, "sk-") || len(secret) <= 3 || len(secret) > 131 || strings.ContainsAny(secret, " \t\r\n\x00,") {
		return result, ErrIAMAPIKeyUnauthorized
	}
	key := strings.TrimPrefix(secret, "sk-")
	// GORM's SQL logger interpolates query parameters. Suppress it for credential
	// lookup, including database failures, and return only classified errors below.
	database := DB.WithContext(ctx).Session(&gorm.Session{Logger: DB.Logger.LogMode(logger.Silent)})
	var token Token
	if err := database.Where(map[string]any{"key": key}).First(&token).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return result, ErrIAMAPIKeyUnauthorized
		}
		return result, errors.New("API key lookup unavailable")
	}
	// Enforce byte equality even on databases with case-insensitive collation.
	if subtle.ConstantTimeCompare([]byte(key), []byte(token.Key)) != 1 || token.ManagementSource != ManagementSourceIAM {
		return result, ErrIAMAPIKeyUnauthorized
	}
	var link IAMAPIKeyLink
	if err := database.Where("token_id = ?", token.Id).First(&link).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return result, ErrIAMAPIKeyUnauthorized
		}
		return result, errors.New("API key mapping unavailable")
	}
	if link.DesiredState != IAMAPIKeyStateActive || link.NewAPIUserID != token.UserId || link.IAMAPIKeyID <= 0 || link.IAMUserID <= 0 || link.LastVersion <= 0 {
		return result, ErrIAMAPIKeyUnauthorized
	}
	var identity IAMIdentityLink
	if err := database.Where("iam_user_id = ?", link.IAMUserID).First(&identity).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return result, ErrIAMAPIKeyAccessDenied
		}
		return result, errors.New("API key identity unavailable")
	}
	if identity.DesiredState != IAMIdentityStateActive || identity.LastVersion < 1 || identity.NewAPIUserID != token.UserId || identity.OrganizationID <= 0 || (identity.OrganizationType != 1 && identity.OrganizationType != 2) {
		return result, ErrIAMAPIKeyAccessDenied
	}
	var user User
	if err := database.First(&user, token.UserId).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return result, ErrIAMAPIKeyAccessDenied
		}
		return result, errors.New("API key user unavailable")
	}
	if user.ManagementSource != ManagementSourceIAM || user.Status != common.UserStatusEnabled || token.Group != "" {
		return result, ErrIAMAPIKeyAccessDenied
	}
	if token.Status != common.TokenStatusEnabled || (token.ExpiredTime != -1 && token.ExpiredTime <= time.Now().Unix()) || (!token.UnlimitedQuota && token.RemainQuota <= 0) {
		return result, ErrIAMAPIKeyUnauthorized
	}
	ip, err := netip.ParseAddr(clientIP)
	if err != nil || ip.Zone() != "" || ip.IsUnspecified() {
		return result, ErrIAMAPIKeyAccessDenied
	}
	limits := token.GetIpLimits()
	if len(limits) > 0 {
		allowed := false
		for _, value := range limits {
			prefix, err := netip.ParsePrefix(value)
			if err != nil {
				return result, ErrIAMAPIKeyAccessDenied
			}
			allowed = allowed || prefix.Contains(ip.Unmap())
		}
		if !allowed {
			return result, ErrIAMAPIKeyAccessDenied
		}
	}
	models := []string{}
	if token.ModelLimitsEnabled {
		models = normalizeStringSet(strings.Split(token.ModelLimits, ","))
		if len(models) == 0 {
			return result, ErrIAMAPIKeyAccessDenied
		}
	}
	return IAMAPIKeyPrincipal{APIKeyID: link.IAMAPIKeyID, IAMUserID: link.IAMUserID,
		APIPlatformUserID: int64(token.UserId), OrganizationID: identity.OrganizationID,
		OrganizationType: identity.OrganizationType, IdentityVersion: identity.LastVersion,
		APIKeyVersion: link.LastVersion, ModelLimits: models}, nil
}
