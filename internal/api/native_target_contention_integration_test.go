//go:build integration

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
	"github.com/stretchr/testify/require"
)

type nativeAdmissionOutcome struct {
	receipt *model.DeploymentAdmissionReceipt
	err     error
}

func TestNativeTargetAdmissionSerializesActualCataloguePatch(t *testing.T) {
	for _, admissionFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "patch first", true: "admission first"}[admissionFirst], func(t *testing.T) {
			db, runner, admission := nativeFirstAdmissionFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			held, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { cancel(); _ = held.Rollback() }()
			var holder int
			require.NoError(t, held.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holder))
			_, err = db.GetRunner(ctx, held, runner.OrgId, runner.Id, model.GetModeForUpdate)
			require.NoError(t, err)
			admitted := make(chan nativeAdmissionOutcome, 1)
			patched := make(chan *httptest.ResponseRecorder, 1)
			router := finalDeletionRouter(t, db)
			startAdmission := func() {
				go func() {
					receipt, err := model.AuthorizeDeploymentAdmission(ctx, db, admission)
					admitted <- nativeAdmissionOutcome{receipt, err}
				}()
			}
			startPatch := func() {
				go func() {
					request := httptest.NewRequest(http.MethodPatch, "/orgs/"+runner.OrgId+"/runners/"+runner.Id, strings.NewReader(`{"runner_configuration":{"type":"native-agent","native_execution":null}}`)).WithContext(ctx)
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("From", userid.InternalSystemUuid.String())
					response := httptest.NewRecorder()
					router.ServeHTTP(response, request)
					patched <- response
				}()
			}
			first, second := startPatch, startAdmission
			if admissionFirst {
				first, second = startAdmission, startPatch
			}
			first()
			pid := waitFinalDeletionLock(t, db, holder, 0)
			second()
			waitFinalDeletionLock(t, db, holder, pid)
			require.NoError(t, held.Commit())
			result := <-admitted
			response := <-patched
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var expected int
			if admissionFirst {
				require.NoError(t, result.err)
				require.NotNil(t, result.receipt)
				expected = 1
				replayed, err := model.AuthorizeDeploymentAdmission(ctx, db, admission)
				require.NoError(t, err)
				require.Equal(t, result.receipt, replayed)
			} else {
				_, conflict := model.IsErrConflict(result.err)
				require.True(t, conflict, "catalogue disable must win: %v", result.err)
				require.Nil(t, result.receipt)
			}
			require.Equal(t, expected, admissionGuards(t, db, admission.OrgID))
			epoch, err := model.ReadEnvironmentAdmissionEpoch(ctx, db, nil, admission.OrgID, admission.EnvironmentUUID)
			require.NoError(t, err)
			require.Equal(t, int64(expected), epoch)
			current, err := db.GetRunner(ctx, nil, runner.OrgId, runner.Id, model.GetModeDefault)
			require.NoError(t, err)
			require.Nil(t, current.RunnerConfiguration["native_execution"])
		})
	}
}

func TestNativeTargetLegacyNullAvoidsRunnerDeletionLockCycle(t *testing.T) {
	db, native, admission := nativeTargetAdmissionFixture(t)
	setup, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = setup.Rollback() }()
	legacy := *native
	legacy.Id, legacy.RunnerType, legacy.RunnerConfiguration = "legacy", "kubernetes", map[string]any{"type": "kubernetes"}
	created, err := db.CreateRunner(t.Context(), setup, &legacy)
	require.NoError(t, err)
	legacy = *created
	require.NoError(t, setup.Commit())
	_, err = db.UpdateEnvironment(t.Context(), nil, admission.OrgID, "project", "development", &model.EnvironmentPatch{RunnerId: opt.Of(legacy.Id), UpdatedAt: time.Now()})
	require.NoError(t, err)
	admission.NativeTarget = nil
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	held, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { cancel(); _ = held.Rollback() }()
	var holder int
	require.NoError(t, held.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holder))
	_, err = db.GetEnvironmentByUuid(ctx, held, admission.OrgID, admission.EnvironmentUUID, model.GetModeForUpdate)
	require.NoError(t, err)
	_, err = db.GetRunner(ctx, held, admission.OrgID, legacy.Id, model.GetModeForUpdate)
	require.NoError(t, err)
	admitted := make(chan nativeAdmissionOutcome, 1)
	go func() {
		receipt, err := model.AuthorizeDeploymentAdmission(ctx, db, admission)
		admitted <- nativeAdmissionOutcome{receipt, err}
	}()
	pid := waitFinalDeletionLock(t, db, holder, 0)
	deleted := make(chan error, 1)
	go func() {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			deleted <- err
			return
		}
		defer func() { _ = tx.Rollback() }()
		deleted <- db.DeleteRunner(ctx, tx, admission.OrgID, legacy.Id)
	}()
	waitFinalDeletionLock(t, db, holder, pid)
	require.NoError(t, held.Commit())
	result, deleteErr := <-admitted, <-deleted
	require.NoError(t, result.err)
	require.Equal(t, int64(1), result.receipt.Epoch)
	_, conflict := model.IsErrConflict(deleteErr)
	require.True(t, conflict, "legacy delete must refuse its assigned Environment, not deadlock: %v", deleteErr)
	current, err := db.GetEnvironmentByUuid(ctx, nil, admission.OrgID, admission.EnvironmentUUID, model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, legacy.Id, current.RunnerId.Must())
	retained, err := db.GetRunner(ctx, nil, admission.OrgID, legacy.Id, model.GetModeDefault)
	require.NoError(t, err)
	require.Equal(t, legacy.Uuid, retained.Uuid)
	require.Equal(t, 1, admissionGuards(t, db, admission.OrgID))
}

func TestNativeTargetAdmissionSerializesEnvironmentReassignment(t *testing.T) {
	for _, admissionFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "reassignment first", true: "admission first"}[admissionFirst], func(t *testing.T) {
			db, runner, admission := nativeFirstAdmissionFixture(t)
			setup, err := db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = setup.Rollback() }()
			other := *runner
			other.Id = "other-native"
			_, err = db.CreateRunner(t.Context(), setup, &other)
			require.NoError(t, err)
			require.NoError(t, setup.Commit())
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			held, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { cancel(); _ = held.Rollback() }()
			var holder int
			require.NoError(t, held.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holder))
			_, err = db.GetEnvironmentByUuid(ctx, held, admission.OrgID, admission.EnvironmentUUID, model.GetModeForUpdate)
			require.NoError(t, err)
			admitted := make(chan nativeAdmissionOutcome, 1)
			reassigned := make(chan error, 1)
			startAdmission := func() {
				go func() {
					receipt, err := model.AuthorizeDeploymentAdmission(ctx, db, admission)
					admitted <- nativeAdmissionOutcome{receipt, err}
				}()
			}
			startReassignment := func() {
				go func() {
					_, err := db.UpdateEnvironment(ctx, nil, admission.OrgID, "project", "native-first", &model.EnvironmentPatch{RunnerId: opt.Of(other.Id), UpdatedAt: time.Now()})
					reassigned <- err
				}()
			}
			first, second := startReassignment, startAdmission
			if admissionFirst {
				first, second = startAdmission, startReassignment
			}
			first()
			pid := waitFinalDeletionLock(t, db, holder, 0)
			second()
			waitFinalDeletionLock(t, db, holder, pid)
			require.NoError(t, held.Commit())
			result := <-admitted
			require.NoError(t, <-reassigned)
			var expected int
			if admissionFirst {
				require.NoError(t, result.err)
				require.NotNil(t, result.receipt)
				expected = 1
			} else {
				_, conflict := model.IsErrConflict(result.err)
				require.True(t, conflict, "reassignment must win: %v", result.err)
				require.Nil(t, result.receipt)
			}
			require.Equal(t, expected, admissionGuards(t, db, admission.OrgID))
			epoch, err := model.ReadEnvironmentAdmissionEpoch(ctx, db, nil, admission.OrgID, admission.EnvironmentUUID)
			require.NoError(t, err)
			require.Equal(t, int64(expected), epoch)
			current, err := db.GetEnvironmentByUuid(ctx, nil, admission.OrgID, admission.EnvironmentUUID, model.GetModeDefault)
			require.NoError(t, err)
			require.Equal(t, other.Id, current.RunnerId.Must())
		})
	}
}
