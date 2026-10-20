#!/bin/sh
set -eu

# Balanced from go test -json timings on the pre-change main revision.
# Keep this list complete so newly added tests cannot silently go unsharded.
assignments='
1 Test014ProjectVocabularyUpgradePreservesTasksSandboxesAndOpenPR
1 TestOfficial014HomeCanBeRepairedAfter015Update
1 TestOfficial015HomeMigratesReviewModeDuringUpdate
1 TestFrozen015LocalReviewPlanKeepsItsEvidenceRequirement
1 TestFullPipelineCanBeFrozenButIsNotReady
1 Test014TaskSnapshotRebuildPreservesMergeAuthorityAndSandbox
1 Test014MalformedConfigRetainsFilenameAndErrorCode
1 Test014ProjectShorthandConfigVocabularyUpgrade
1 TestFailedRecoveryBoundWithRealHerdr
1 TestE2ELockOwnerCannotOutliveTestProcess
1 TestTeardownStopsDetachedTaskServer
1 TestTeardownPreservesDetachedSSHControlMaster
1 TestTeardownStopsProcessStartedThroughHerdrServer
1 TestStoppedWorkerPaneCanBeRelaunched
1 TestProjectCommandDoesNotWaitForeverForStoppedLockOwner
1 TestReviewRejectsCaseVariantCodexModelKeysAtSpawnAndRelaunch
1 TestWatchRuntimeDiscoversAndTearsDownMergedPR
1 TestPRPollPartialGraphQLFailureDoesNotStarveMergedWorker
1 TestMergedPRWithIgnoredArtifactStillMerges
1 TestReviewRejectsQuotedSameModelOverride
1 TestExistingSandboxWorktreeOpenAndLockSpike
1 TestHistoricalMultiIssuePublishKeepsClosingLinks
1 TestPublishRefreshesReusedPRMetadata
1 TestRetitleThenPublishKeepsTheConventionalPRTitle
1 TestQueuedMessageBlocksResearchDoneUntilInstructionApplied
1 TestRelaunchedShowExplainsMixedMessageStatuses
1 TestLeadWatchOwnsNoticeUntilItExits
1 TestConcurrentWatchSignalsAndLeadCommandsNeverReturnStoreBusy
1 TestIngestDoesNotWaitForHealthyWatchRuntimeLock
1 TestIngestRetriesWatchRepairWithinBudgetAfterLockContention
1 TestWatchAckCommitsBeforeUnrelatedTeardown
1 TestWatchRepairsAfterHerdrAgentEvent
1 TestConcurrentProjectCommandsStartOneWatch
1 TestWatchRuntimeIdentityIsStoredInProjectRuntime
1 TestWatchKeepsWatchingAfterRetryableForgeProbeTimeout
1 TestTimedOutWatcherGitReadDoesNotLockWorktreeOrFailWatch
1 TestWatchReportsPersistentForgeProbeFailureAfterThreePolls
1 TestStoppingBackgroundWatchReleasesActiveForgePoll
1 TestWorkerGuardAgainstIsolatedHerdr
1 TestStartupAndExplicitRecoveryDoNotDoubleRelaunch
1 TestRecoveryRepairsWorkersFromAnInterruptedGroupEpisode
1 TestPiStreamDisconnectAutomaticallyResumesWithinBound
1 TestPiStreamDisconnectTailBeginningInsideToolPanelAutomaticallyResumes
1 TestT242OrdinaryToolOutputIsNotRetried
1 TestT242QuotedRefusalTranscriptIsNotRetried
1 TestT242IdenticalRepaintDoesNotStrandRecovery
1 TestT242RealPiErrorUIIsRecognized
1 TestT247IdleObservationBeforeEndHookDoesNotStrand
1 TestPublishRemembersUnverifiedPrePushHeadAcrossRestart
1 TestPublishRejectsUnexpectedHeadDespitePersistedPrePushEvidence
1 TestUpStartsLeadAndSettlesRelaunchIntentWhileRecoveryWaitsForBackoff
1 TestUpStartsLeadAndSettlesRelaunchIntentDuringRestartBackoff
1 TestFailedGroupAttemptRetriesAfterLeadStarts
1 TestSelectedReviewPolicyLocalMerge
1 TestTaskPreferencesRequireScopedUserChoice
1 TestPermissionTemplatesRequireConsentThroughCLI
1 TestSetupWithoutHerdrServer
1 TestBusyPiSteerPastQueuedMessageAndInterruptE2E
1 TestExpiredUncertainSubmissionDoesNotBlockFreshDeliveryE2E
1 TestUnsendDoesNotDeliverQueuedMessageE2E
1 TestUnsendCannotWithdrawPromptAppliedBeforeHerdrError
1 TestUnsendRaceNeverReportsWithdrawnAfterDelivery
1 TestSendPreflightIsBoundedAgainstBlockedForge
1 TestQueuedSendIsBoundedAndShowsQueueAge
1 TestDoneSignalRefusesHeadWithFailedRequiredChecks
1 TestDoneSignalRefusesTurnFromBeforeDeliveredInstruction
1 TestDoneSignalRefusesStaleWorkerInstanceAfterRelaunch
1 TestRealPiInterruptStopsBusyTurnE2E
1 TestT229RebuildPreservesStoppedProject
2 Test014LeadAdapterVocabularyHandoffAndReceipt
2 Test014LookoutReexecReportsReplacement
2 TestSupervised014PollerReexecsThroughWatchCompatibility
2 TestLegacyWorkerTabsAfterWorktreeUpgrade
2 TestTeardownBlockedNoticeNamesForeignSandboxServer
2 TestNoticeHandoffSurvivesHarnessCrashAndAvoidsRecordedReplay
2 TestWatchHandoffRepairsDeadPoller
2 TestObservationWorkloadDiscoversForgeResultsWithoutModelTurns
2 TestObservationWorkloadCountersAreDeterministic
2 TestPiExtensionSessionReplacementOwnsOneWatchGeneration
2 TestClaudeStopOverlapDeadlineAndGracefulStop
2 TestRealClaudeWatchBackgroundLimit
2 TestRealClaudeBackgroundWatchWakesOnlyForNotice
2 TestAcknowledgedNoticeReceiptSurvivesCrashAndRebuild
2 TestOpenCodeAcknowledgedReceiptRecoversAfterLeaseExpiry
2 TestOpenCodeQueuesNoticeBatchDuringBusyTurn
2 TestOpenCodePromptOutcomesFollowReceiptEvidence
2 TestNoticeReceiptsSurviveProjectMoveAndRebuild
2 TestNoticeReceiptRebuildAcceptsRenamedProjectSnapshotIdentity
2 TestNoticeReceiptRebuildRejectsReusedProjectID
2 TestAcceptedNoticeReceiptSurvivesColdRebuild
2 TestGroupCloseDuringTeardownRestoresSurvivingWorker
2 TestLeadHarnessAndRepositoryWorkerSidebar
2 TestAutoRecoverOffHoldsGroupCloseUntilUp
2 TestAuthorRelaunchResumeModelOverrideIsRecordedUnknown
2 TestSelectedReviewPolicyNoneKeepsGateAndMergeAuthority
2 TestPRMergeBindsHeadObservationAndLocalVerdict
2 TestTaskPreferenceRefusalsHaveNoTaskEffects
2 TestAdmittedPlanFreezesRelaunchConfiguration
2 TestWorkflowProfileAdmissionAndNoProfileDirectPath
2 TestAdmittedPublishUsesSelectedBriefInsteadOfMutableCopy
2 TestRelaunchProfileSwitchDoesNotResumeAnotherHarnessSession
2 TestReviewResumeTemplateRejectsSecondSessionSelector
2 TestReviewRejectsCodexProfileSelectorAtSpawnAndRelaunch
2 TestWorkspaceProjectRegistersStartsAndMergesAcrossMembers
2 TestPRMergeAcceptsFollowUpBeforeFailureNotice
2 TestT283DirectPaneWorkReopensCompletedChange
2 TestDoneWorkerClosingReplyDoesNotReopenTask
2 TestQueuedDeliveryAfterDoneDoesNotLookLikeExternalResume
2 TestAckedClaudeWakeResolvesReceiptInSameTurn
2 TestMergedPRAutoTearsDownEvenWhenAutoTeardownIsNever
2 TestExpiredQueuedMessageCrashRaisesNotice
2 TestFirstOutcomeFreshHome
2 TestLaunchSkillsAreInjectedOnlyIntoPosseSessions
2 TestDownRefusesRunningTasks
2 TestReviewRejectsCodexTOMLModelLiteralAndCommentsAtSpawnAndRelaunch
2 TestLocalMergeReportsConcurrentTeardownOutcomeExactlyOnce
2 TestRecoveryCrashConsumesFinalAttempt
2 TestRecoveryRetriesMissingWorkerMarkedRecoveredInSameGroupEpisode
2 TestAutomaticRecoveryPreservesLateFailedSignal
2 TestFailedMergeIntentDoesNotPoisonNextCLICommand
2 TestT229RecreatedDatabaseProjectIDCollision
2 TestT229LegacyWorkspaceRebuild
2 TestT233MovedProjectRebuild
2 TestPublishRetainsRepeatedMetadataFlagsOnCreateAndRefresh
2 TestPiProviderRefusalIsNotRetried
2 TestT242DecisionWaitIsNotRetried
2 TestT242DecisionDuringBackoffIsNotRetried
2 TestT242InterruptedIngestRecoversOrRaisesNotice
2 TestT242RealPiRefusalUIIsNotRetried
2 TestT247SuccessfulGrepOfPiErrorResultIsNotRetried
2 TestWorkspacePublishRetriesAfterTimeoutForUnverifiedPrePushHead
2 TestWorkspacePublishRemembersUnverifiedPrePushHeadAcrossRestart
3 TestWorkersAsGroupedChildrenRecoverAfterAnotherPrimaryClosesGroup
3 TestCleanDryRunAndReclaimsFinishedTask
3 TestPublishRetriesAfterTimeoutForUnverifiedPrePushHead
3 TestWorkspaceResearchTeardownPreservesMemberAndRootAttachments
3 TestMergedPRSnapshotsUnmergedFollowUp
3 TestListRecoversRestartWithoutStartupHook
3 TestTaskNamesBranchesAndLegacyRecoveryRemainIndependent
3 TestSignalFilesAcceptOwnTaskScratchAndRejectOtherRoots
3 TestSelectedReviewPolicyNoMistakesFailsClosed
3 TestSelectedReviewPolicyWorkspaceAdmission
3 TestNoMistakesReceiptGatesPRMergeAtExactHead
3 TestAuthorizedFeatureBranchChoiceKeepsDefaultAndSurvivesRestart
3 TestWorkerAdmittedStatusDoesNotRecoverOrExposeSibling
3 TestDownKeepsProjectStoppedUntilUp
3 TestT146RealHerdrForeignAgentInReusedPaneSurvivesDiscard
3 TestT175OwnedToForeignSameKindRestartDuringProcessInspectionIsPreserved
3 TestT175ForeignRestartDuringDiscardOwnershipCheckIsPreserved
3 TestTeardownNeverClosesAReplacementAtThePaneCloseBoundary
3 TestTeardownStopsRetainedShellByVerifiedIdentity
3 TestTeardownPidfdLauncherHelper
3 TestWorkerExitedDoneTaskWithOpenPRCanBeDiscarded
3 TestTeardownWithoutPidfdPreservesResourcesAndReportsCapability
3 TestWatchRestartsWithoutRecordedGeneration
3 TestMergeReadyBeforeMergeDoesNotRaiseLatePROpened
3 TestWorkspaceMergeReadyNoticeIsDeduplicatedPerHeadAndCompletion
3 TestWorkspacePRReadinessDoesNotOutliveDoneState
3 TestMergeReadyDecisionCanDiscardUnmergedPR
3 TestWatchCloseObservationDuringDiscardTeardownIsObsoleted
3 TestMergeInterruptsFollowUpAndReleasesWorker
3 TestMergedPRHasExactlyOneVisibleTeardownReason
3 TestConcurrentWatchTeardownRaisesNoFalseReason
3 TestReviewRejectsExplicitAuthorSessionResume
3 TestFocusedWorkerLaunch
3 TestBuiltInInterruptKeysVerifyStoppedWorkerForEachKindE2E
3 TestPiStreamDisconnectIsNotNudgedIntoFocusedWorker
3 TestPiUnprefixedStreamDisconnectAfterEditAutomaticallyResumes
3 TestT242QueuedLeadInstructionWins
3 TestT242BackoffIsPersistedAndBounded
3 TestT247RealPiSuccessfulGrepIsNotRetried
3 TestGroupCloseDuringStartRecoversLeadAndSandbox
3 TestWatchWaitHonorsTimeoutAndSIGTERMWithoutVisibleShell
3 TestHiddenWatchRuntimeDeliversNoticesWhenLeadHasNoWait
3 TestHiddenWatchUsesNamedHerdrSession
3 TestWatchDoesNotOpenLoginShellOrVisiblePane
3 TestPassiveWatchLeavesCommandBoundaryIntentsForNormalRecovery
3 TestWatchHeartbeatStopsWhenProjectMaintenanceStalls
3 TestWatchPollerUsesItsSpawnedBinaryInsteadOfSetupManifest
3 TestWatchRuntimeReexecsAfterBinaryReplacement
3 TestWatchRuntimeStaleHeartbeatRaisesOneNoticeAndRepairs
3 TestWatchRuntimeHeartbeatProgressClearsAnApparentStaleReading
3 TestWatchRepairsDeadWatchesWithoutStaleNotices
3 TestForgeReadinessUsesCacheInMultiMemberWorkspace
3 TestUpStartsRegisteredWorkspaceWithoutRepositoryOrForgeCalls
3 TestPostSignalMixedContentionAndPersistentFailureIsVisible
3 TestDownDoesNotSignalDifferentMainPackageInPosseModule
3 TestDecisionCLIFromLeadAndUserShellDeliversAnswerToLead
3 TestFixtureRootReclaimsOnlyAbandonedOwnedRoots
3 TestFixtureRootReclaimsOnlyConfiguredPrefix
3 TestIsolatedHerdrServerDoesNotOutliveTest
3 TestIsolatedHerdrTerminationHelper
3 TestIsolatedHerdrAcceptsDefaultTaskScratchTMPDIR
3 TestFixtureRootPreservesLiveIsolatedProcesses
3 TestSidebarCaptureReplaysSparseUpdatesAfterEmptyBootstrap
3 TestFixtureRootRemovesReadOnlyGoModuleDirectories
4 TestDownStopsAWedgedProjectWatch
4 TestReviewRejectsAttachedSessionSelectors
4 TestB05ConfigDispatchAndFrozenRelaunch
4 TestPosseSpawnNoticeMergeTeardownAndRecovery
4 TestPosseInPlaceDirectoryProjects
4 TestRealCLIIntentCrashMatrix
4 TestGroupCloseDuringRelaunchRestoresLeadAndWorker
4 TestWatchRestartsAfterProcessExit
4 TestPRMergeLifecycleAndExternalMerge
4 TestSelectedReviewPolicyConfigNeedsUserConsent
4 TestReviewPolicyLegacyConfigMigration
4 Test015ReviewModeUpgradeMigratesLocalWithoutTouchingMergeMode
4 TestExternalMergeWithoutEvidenceIsPolicyViolation
4 TestWorkspacePreferenceCompatibilityPrecedesBranchEffects
4 TestPRMergeRefusesBranchBehindLatestTarget
4 TestFailedRecoveryIsBoundedUnderPluginEvents
4 TestWorkerPublishRetriesLaggingOpenPRHead
4 TestWorkerPublishDistinguishesMovedHeadFromLagTimeout
4 TestApprovedLeadPreferencesMove
4 TestStartRetriesRetryableStoreBusyBeforeTaskCreation
4 TestExternalMergeDuringFollowUpWithMovedTaskBranch
4 TestWatchRestartsAfterHerdrRestart
4 TestWatchRepairRestoresHeldProjectWithoutExecutorRecovery
4 TestUpClosesLegacyVisibleWatchTab
4 TestReviewRejectsAttachedCodexModelConfig
4 TestWorkflowRunsRequireUserApprovedProgressionAndRecoverPosition
4 TestReviewRejectsMalformedModelAliasOverride
4 TestPRCreateCrashRecoveryAdoptsOpenPullRequest
4 TestExplicitRelaunchSettlesPendingRecovery
4 TestUpDefersInterruptedOpenPRRecoveryUntilLeadStarts
4 TestDownDoesNotSignalUnrelatedExecutableNamedWatch
4 TestCodexCaseVariantModelKeysKeepLoopbackModelAtDefault
4 TestStartCrashImmediatelyAfterTaskCreationIsRecovered
4 TestPruneRechecksScratchAfterConcurrentStart
4 TestPruneReclaimsOrphanOpenCacheStorage
4 TestTeardownStopsTaskProcessesAndPruneProtectsHeldBranches
4 TestPrunePreservesBranchAdvancedAfterFinalCheck
4 TestPruneReclaimsMappedCacheStorage
4 TestWorkerMappingIsStoppedBeforeRelease
4 TestShowWaitsForDeferredTeardown
4 TestPrunePreservesBranchCheckedOutAfterFinalCheck
4 TestWatchKeepsHealthyMemberWorkWhenOtherForgeAndFetchStall
4 TestWatchHeartbeatAdvancesBetweenSerialSlowPRFetches
4 TestT229SignalSnapshotFailure
4 TestWorkspaceFetchTimeoutNoticeCoalescesAndResets
4 TestListSucceedsDuringTransientObservationContention
4 TestDoneTaskWithVanishedPaneCanBeDiscarded
4 TestApprovedDiscardStopsWorkingWorker
4 TestApprovedDiscardStopsReopenedDoneWorker
4 TestApprovedDiscardPreservesSameNamedReplacementBeforeSignal
4 TestApprovedWorkingDiscardResumesAfterCrash
4 TestApprovedDiscardClosesAndVerifiesRecordedPR
4 TestApprovedDiscardKeepsSandboxHeldWhenPRCloseFails
4 TestApprovedDiscardRefusesMergedPR
4 TestClosedPRDiscardRechecksForgeStateBeforeTeardown
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
count=${POSSE_E2E_COUNT:-1}
case "$count" in
  ''|*[!0-9]*|0) echo "POSSE_E2E_COUNT must be a positive integer" >&2; exit 2 ;;
esac
timeout=${POSSE_E2E_TIMEOUT:-15m}
shard=$1
case "$shard" in
  1|2|3|4) ;;
  *) echo "unknown E2E shard: $shard" >&2; exit 2 ;;
esac

listed=$(go test -tags=e2e -list '^Test' ./internal/e2e)
validate_assignments "$listed" "$assignments"
pattern=$(printf '%s\n' "$assignments" | awk -v shard="$shard" '$1 == shard { if (count++) printf "|"; printf "%s", $2 } END { print "" }')
printf 'Running E2E shard %s/4\n' "$shard"
# Recovery-heavy shards take up to eleven minutes locally; allow bounded runner variance.
go test -tags=e2e -count="$count" -timeout="$timeout" ./internal/e2e -run "^(${pattern})$"
