#!/bin/sh
set -eu

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
1 TestPublishRefreshesReusedPRMetadata
1 TestQueuedMessageDoesNotStayQueuedWhenScoutReportsDone
1 TestRelaunchedShowExplainsMixedMessageStatuses
1 TestLeadLookoutOwnsNoticeUntilItExits
1 TestConcurrentLookoutSignalsAndLeadCommandsNeverReturnStoreBusy
1 TestLookoutAckCommitsBeforeUnrelatedTeardown
1 TestWorkerGuardAgainstIsolatedHerdr
1 TestStartupAndExplicitRecoveryDoNotDoubleRelaunch
1 TestRecoveryRepairsRidersFromAnInterruptedGroupEpisode
1 TestUpStartsLeadAndSettlesRelaunchIntentWhileRecoveryWaitsForBackoff
1 TestFailedGroupAttemptRetriesAfterLeadStarts
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
2 TestRecoveryRetriesMissingRiderMarkedRecoveredInSameGroupEpisode
2 TestAutomaticRecoveryPreservesLateFailedSignal
2 TestFailedLandIntentDoesNotPoisonNextCLICommand
2 TestPublishRetainsRepeatedMetadataFlagsOnCreateAndRefresh
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
3 TestForgeReadinessUsesCacheInMultiMemberWorkspace
3 TestUpStartsRegisteredWorkspaceWithoutRepositoryOrForgeCalls
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
4 TestWorkerPublishDistinguishesMovedHeadFromLagTimeout
4 TestApprovedLeadPreferencesMove
4 TestRideRetriesRetryableStoreBusyBeforeTaskCreation
4 TestExternalMergeDuringFollowUpWithMovedTaskBranch
4 TestLookoutRestartsAfterHerdrRestart
4 TestReviewRejectsAttachedCodexModelConfig
4 TestReviewRejectsMalformedModelAliasOverride
4 TestLookoutDoesNotRecreateDuringSlowLoginShellStartup
4 TestPRCreateCrashRecoveryAdoptsOpenPullRequest
4 TestExplicitRelaunchSettlesPendingRecovery
4 TestLookoutRecoveryBackoffSurvivesLeadRearm
4 TestUpDefersInterruptedOpenPRRecoveryUntilLeadStarts
4 TestDownDoesNotSignalUnrelatedExecutableNamedLookout
4 TestCodexCaseVariantModelKeysKeepLoopbackModelAtDefault
4 TestRideCrashImmediatelyAfterTaskCreationIsRecovered
4 TestLookoutKeepsHealthyMemberWorkWhenOtherForgeAndFetchStall
'

validate_assignments() {
  discovered=$1
  manifest=$2

  if ! printf '%s\n' "$manifest" | awk '
    NF == 0 { next }
    NF != 2 {
      printf "invalid E2E shard assignment on manifest line %d: expected <shard> <test>\n", NR > "/dev/stderr"
      invalid = 1
      next
    }
    {
      if ($1 != "1" && $1 != "2" && $1 != "3" && $1 != "4") {
        printf "invalid E2E shard ID %s on manifest line %d; expected 1..4\n", $1, NR > "/dev/stderr"
        invalid = 1
      }
      if ($2 !~ /^Test/) {
        printf "invalid top-level E2E test name %s on manifest line %d\n", $2, NR > "/dev/stderr"
        invalid = 1
      }
      if (++seen[$2] > 1) {
        printf "duplicate E2E test assignment: %s\n", $2 > "/dev/stderr"
        invalid = 1
      }
    }
    END { if (invalid) exit 1 }
  '; then
    return 1
  fi

  discovered_tests=$(printf '%s\n' "$discovered" | awk 'NF == 1 && $1 ~ /^Test/ { print $1 }' | sort)
  assigned_tests=$(printf '%s\n' "$manifest" | awk 'NF == 2 { print $2 }' | sort)
  if [ "$discovered_tests" != "$assigned_tests" ]; then
    echo "E2E shard assignments must include every discovered top-level test exactly once." >&2
    printf 'Discovered tests:\n%s\nAssigned tests:\n%s\n' "$discovered_tests" "$assigned_tests" >&2
    return 1
  fi
}

run_self_check() {
  discovered=$(go test -tags=e2e -list '^Test' ./internal/e2e)
  validate_assignments "$discovered" "$assignments"

  if validate_assignments 'TestAssigned
TestΩUnassigned' '1 TestAssigned' >/dev/null 2>&1; then
    echo "self-check failed: an unassigned Unicode test was accepted" >&2
    return 1
  fi
  if ! validate_assignments 'TestΩUnassigned' '2 TestΩUnassigned'; then
    echo "self-check failed: a valid Unicode test assignment was rejected" >&2
    return 1
  fi
  if validate_assignments 'TestFixtureRootRemovesReadOnlyGoModuleDirectories' '5 TestFixtureRootRemovesReadOnlyGoModuleDirectories' >/dev/null 2>&1; then
    echo "self-check failed: an invalid manifest shard ID was accepted" >&2
    return 1
  fi
  if validate_assignments 'TestAssigned' '1 TestAssigned
2 TestAssigned' >/dev/null 2>&1; then
    echo "self-check failed: a duplicate test assignment was accepted" >&2
    return 1
  fi
  printf 'E2E shard assignment self-check passed.\n'
}

if [ "$#" -eq 1 ] && [ "$1" = --self-test ]; then
  run_self_check
  exit 0
fi
if [ "$#" -ne 1 ]; then
  echo "usage: $0 <1|2|3|4>|--self-test" >&2
  exit 2
fi
shard=$1
case "$shard" in
  1|2|3|4) ;;
  *) echo "unknown E2E shard: $shard" >&2; exit 2 ;;
esac

listed=$(go test -tags=e2e -list '^Test' ./internal/e2e)
validate_assignments "$listed" "$assignments"
pattern=$(printf '%s\n' "$assignments" | awk -v shard="$shard" '$1 == shard { if (count++) printf "|"; printf "%s", $2 } END { print "" }')
printf 'Running E2E shard %s/4\n' "$shard"
go test -tags=e2e -count=1 -timeout=8m ./internal/e2e -run "^(${pattern})$"
