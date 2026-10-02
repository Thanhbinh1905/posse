#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <1|2|3|4>" >&2
  exit 2
fi
shard=$1
case "$shard" in
  1|2|3|4) ;;
  *) echo "unknown E2E shard: $shard" >&2; exit 2 ;;
esac

# Balanced from go test -json timings on the pre-change main revision.
# Keep this list complete so newly added tests cannot silently go unsharded.
assignments='
1 TestFailedRecoveryBoundWithRealHerdr
1 TestStoppedRiderPaneCanBeRelaunched
1 TestProjectCommandDoesNotWaitForeverForStoppedLockOwner
1 TestReviewRejectsCaseVariantCodexModelKeysAtSpawnAndRelaunch
1 TestLookoutTabPollsAndTearsDownMergedPR
1 TestPRPollPartialGraphQLFailureDoesNotStarveMergedRider
1 TestMergedPRWithIgnoredArtifactStillLands
1 TestReviewRejectsQuotedSameModelOverride
1 TestExistingMountWorktreeOpenAndLockSpike
1 TestHistoricalMultiIssuePublishKeepsClosingLinks
1 TestLeadLookoutOwnsNoticeUntilItExits
1 TestWorkerGuardAgainstIsolatedHerdr
1 TestStartupAndExplicitRecoveryDoNotDoubleRelaunch
1 TestRecoveryRepairsRidersFromAnInterruptedGroupEpisode
1 TestSetupWithoutHerdrServer
2 TestLegacyRiderTabsAfterWorktreeUpgrade
2 TestGroupCloseDuringTeardownRestoresSurvivingRider
2 TestLeadHarnessAndRepositoryRiderSidebar
2 TestAutoRecoverOffHoldsGroupCloseUntilUp
2 TestAuthorRelaunchResumeModelOverrideIsRecordedUnknown
2 TestReviewResumeTemplateRejectsSecondSessionSelector
2 TestReviewRejectsCodexProfileSelectorAtSpawnAndRelaunch
2 TestWorkspaceProjectRegistersRidesAndLandsAcrossMembers
2 TestPRLandingAcceptsFollowUpBeforeFailureNotice
2 TestMergedPRAutoTearsDownEvenWhenAutoUnsaddleIsNever
2 TestExpiredQueuedMessageCrashRaisesNotice
2 TestFirstOutcomeFreshHome
2 TestDownRefusesRunningTasks
2 TestReviewRejectsCodexTOMLModelLiteralAndCommentsAtSpawnAndRelaunch
2 TestLocalLandReportsConcurrentTeardownOutcomeExactlyOnce
2 TestRecoveryCrashConsumesFinalAttempt
2 TestAutomaticRecoveryPreservesLateFailedSignal
2 TestFailedLandIntentDoesNotPoisonNextCLICommand
3 TestRidersAsGroupedChildrenRecoverAfterAnotherPrimaryClosesGroup
3 TestWorkspaceScoutTeardownPreservesMemberAndRootAttachments
3 TestMergedPRSnapshotsUnmergedFollowUp
3 TestRosterRecoversRestartWithoutStartupHook
3 TestDownKeepsProjectStoppedUntilUp
3 TestT146RealHerdrForeignAgentInReusedPaneSurvivesDiscard
3 TestLookoutRestartsWithoutRecordedGeneration
3 TestLandReadyBeforeMergeDoesNotRaiseLatePROpened
3 TestMergeInterruptsFollowUpAndReleasesRider
3 TestMergedPRHasExactlyOneVisibleTeardownReason
3 TestConcurrentLookoutTeardownRaisesNoFalseReason
3 TestReviewRejectsExplicitAuthorSessionResume
3 TestFocusedRiderLaunch
3 TestGroupCloseDuringRideRecoversLeadAndMount
3 TestLookoutStartupGraceHonorsTimeoutAndSIGTERM
3 TestForgeProbeTimeoutInMultiMemberWorkspace
3 TestDownDoesNotSignalDifferentMainPackageInPosseModule
3 TestDecisionCLIFromLeadAndUserShellDeliversAnswerToLead
3 TestFixtureRootReclaimsOnlyAbandonedOwnedRoots
3 TestFixtureRootReclaimsOnlyConfiguredPrefix
3 TestFixtureRootPreservesLiveIsolatedProcesses
3 TestSidebarCaptureReplaysSparseUpdatesAfterEmptyBootstrap
3 TestFixtureRootRemovesReadOnlyGoModuleDirectories
4 TestReviewRejectsAttachedSessionSelectors
4 TestPosseSpawnNoticeLandTeardownAndRecovery
4 TestRealCLIIntentCrashMatrix
4 TestGroupCloseDuringRelaunchRestoresLeadAndRider
4 TestLookoutRestartsAfterProcessExit
4 TestPRLandingLifecycleAndExternalMerge
4 TestFailedRecoveryIsBoundedUnderPluginEvents
4 TestWorkerPublishRetriesLaggingOpenPRHead
4 TestExternalMergeDuringFollowUpWithMovedTaskBranch
4 TestLookoutRestartsAfterHerdrRestart
4 TestReviewRejectsAttachedCodexModelConfig
4 TestReviewRejectsMalformedModelAliasOverride
4 TestLookoutDoesNotRecreateDuringSlowLoginShellStartup
4 TestPRCreateCrashRecoveryAdoptsOpenPullRequest
4 TestExplicitRelaunchSettlesPendingRecovery
4 TestLookoutRecoveryBackoffSurvivesLeadRearm
4 TestDownDoesNotSignalUnrelatedExecutableNamedLookout
4 TestCodexCaseVariantModelKeysKeepLoopbackModelAtDefault
4 TestRideCrashImmediatelyAfterTaskCreationIsRecovered
'

listed=$(go test -tags=e2e -list '^Test' ./internal/e2e)
listed_tests=$(printf '%s\n' "$listed" | awk '/^Test[A-Za-z0-9_]*$/ { print }' | sort)
assigned_tests=$(printf '%s\n' "$assignments" | awk 'NF == 2 { print $2 }' | sort)
if [ "$listed_tests" != "$assigned_tests" ]; then
  echo "E2E shard assignments do not match the package's top-level tests; update $0." >&2
  exit 1
fi

pattern=$(printf '%s\n' "$assignments" | awk -v shard="$shard" '$1 == shard { if (count++) printf "|"; printf "%s", $2 } END { print "" }')
printf 'Running E2E shard %s/4\n' "$shard"
go test -tags=e2e -count=1 -timeout=8m ./internal/e2e -run "^(${pattern})$"
