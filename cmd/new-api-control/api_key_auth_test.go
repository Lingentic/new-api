package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestControlAPIKeyAuthentication(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Token{}, &model.IAMIdentityLink{}, &model.IAMAPIKeyLink{}))
	previous := model.DB
	model.DB = database
	t.Cleanup(func() { model.DB = previous; _ = sqlDB.Close() })
	ips := "203.0.113.0/24"
	require.NoError(t, database.Create(&model.User{Id: 1, Username: "fixture", Status: common.UserStatusEnabled, ManagementSource: model.ManagementSourceIAM}).Error)
	require.NoError(t, database.Create(&model.Token{Id: 2, UserId: 1, Key: "fixture-http-key", Status: common.TokenStatusEnabled, ManagementSource: model.ManagementSourceIAM, ExpiredTime: -1, UnlimitedQuota: true, AllowIps: &ips}).Error)
	require.NoError(t, database.Create(&model.IAMIdentityLink{IAMUserID: 11, NewAPIUserID: 1, OrganizationID: 21, OrganizationType: 2, DesiredState: model.IAMIdentityStateActive, LastVersion: 1}).Error)
	require.NoError(t, database.Create(&model.IAMAPIKeyLink{IAMAPIKeyID: 31, IAMUserID: 11, NewAPIUserID: 1, TokenID: 2, DesiredState: model.IAMAPIKeyStateActive, LastVersion: 1}).Error)
	const body = `{"secret":"sk-fixture-http-key","client_ip":"203.0.113.8"}`
	for _, test := range []struct {
		name, method, body, query, header string
		status                            int
	}{
		{"valid", "POST", body, "", "", 200},
		{"missing key", "POST", `{"secret":"","client_ip":"203.0.113.8"}`, "", "", 401},
		{"unknown key", "POST", strings.ReplaceAll(body, "fixture-http-key", "unknown-key"), "", "", 401},
		{"IP denied", "POST", strings.ReplaceAll(body, "203.0.113.8", "198.51.100.2"), "", "", 403},
		{"query rejected", "POST", body, "?secret=sk-fixture-http-key", "", 400},
		{"authorization rejected", "POST", body, "", "Authorization", 400},
		{"identity header rejected", "POST", body, "", "X-IAM-User-ID", 400},
		{"unknown identity field", "POST", `{"secret":"sk-fixture-http-key","client_ip":"203.0.113.8","user_id":"99"}`, "", "", 400},
		{"missing field", "POST", `{"secret":"sk-fixture-http-key"}`, "", "", 400},
		{"invalid JSON", "POST", "{", "", "", 400},
		// GET matches the existing /api-keys/{apiKeyID} state route and must
		// reject "authenticate" as an invalid ID without authenticating a key.
		{"GET cannot authenticate", "GET", body, "", "", 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "/internal/v1/api-keys/authenticate"+test.query, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			if test.header != "" {
				request.Header.Set(test.header, "fixture")
			}
			response := httptest.NewRecorder()
			routesWithPricingDependencies(pricingRouteDependencies{}).ServeHTTP(response, request)
			require.Equal(t, test.status, response.Code)
			require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			require.NotContains(t, response.Body.String(), "fixture-http-key")
			if test.status == 200 {
				var payload struct {
					Success bool                     `json:"success"`
					Data    model.IAMAPIKeyPrincipal `json:"data"`
				}
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
				require.True(t, payload.Success)
				require.EqualValues(t, 11, payload.Data.IAMUserID)
				require.EqualValues(t, 31, payload.Data.APIKeyID)
				require.NotNil(t, payload.Data.ModelLimits)
			}
		})
	}
	// Database failures remain availability errors without interpolated secrets.
	require.NoError(t, sqlDB.Close())
	request := httptest.NewRequest("POST", "/internal/v1/api-keys/authenticate", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	routesWithPricingDependencies(pricingRouteDependencies{}).ServeHTTP(response, request)
	require.Equal(t, 503, response.Code)
	require.NotContains(t, response.Body.String(), "fixture-http-key")
}
