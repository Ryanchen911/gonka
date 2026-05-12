# Maintenance Window: Known Bugs

Three bugs in the maintenance-window feature (PR #998). Bugs 1 and 2 concern maintenance windows
that span multiple epochs; Bug 3 concerns toggling the `maintenance_enabled` governance parameter
while a window is active. Failing regression tests for all three are committed alongside this
document.

## Summary

| | Bug 1 | Bug 2 | Bug 3 |
|---|---|---|---|
| Where | `msg_server_claim_rewards.go` — `hasSignificantMissedValidations` | `maintenance_validation.go` — `checkEpochPhaseOverlap` | `maintenance_lifecycle.go` — `ProcessMaintenanceTransitions` |
| Description | The claim-rewards "missed-validation" exemption checks only the window's start epoch, not the full range of epochs the window covers. | The PoC/DKG phase-overlap check examines only a fixed number of upcoming epochs; a window long enough to extend past that horizon may overlap a critical phase in an unexamined epoch without being rejected. | If governance disables the feature while a window is active, that window's COMPLETE transition is never processed, and the reservation remains in the ACTIVE state indefinitely. |
| Impact | For a multi-epoch window, claiming any epoch other than the start epoch is incorrectly classified as "missed validations" and fails with `Amount=0`, contradicting the proposal's statement that maintenance does not pause ordinary reward eligibility. | A window that overlaps an epoch-critical phase prohibited by the proposal is accepted; this is also the practical precondition for Bug 1. | The participant is treated as permanently in maintenance and is never jailed or slashed for downtime; the state cannot be cleared without a state migration. |
| Test | `TestMsgServer_ClaimRewards_MaintenanceWindowExemptsValidationAcrossEpochRange` | `TestScheduleMaintenance_RejectsPoCOverlapBeyondFixedFutureEpochScan` | `TestLifecycle_ActiveReservationCompletesWhenMaintenanceDisabled` |

## Background: epoch geometry

An epoch begins with a "busy" region (PoC generation and exchange, PoC validation, and the
validator-set switch / DKG), followed by a larger "free" region used for normal inference serving.
Maintenance windows are only meaningful within the free region; the proposal prohibits overlapping
the PoC commit/exchange phase and the DKG phase.

The relevant invariant: an epoch's first block is the start of its busy region, so the epoch
boundary itself lies within a busy region. Any window that crosses an epoch boundary therefore
necessarily overlaps the next epoch's busy region and should be rejected by the overlap check. A
cross-epoch window being accepted is itself a symptom of Bug 2.

## Bug 1 — claim-rewards validation exemption covers only the window's start epoch

A participant schedules a maintenance window spanning epochs N through N+k. Claiming epoch N's
rewards succeeds; claiming epochs N+1 through N+k fails, because during those epochs the participant
was offline, performed none of its assigned validation duties, fails the missed-validation test, and
the exemption did not apply. The claim returns `ErrValidationsMissed` with `Amount=0`.

Root cause: an inconsistency between two code paths that answer the same question — "is this epoch
covered by the participant's maintenance window?". The credit-grant path answers it using the full
epoch range covered by the window (together with an in-progress check); the reward-claim path
answers it using only the window's start epoch. The two should share the same logic.

Preconditions: (1) the participant has activated a multi-epoch window; (2) the mid and end epochs of
the window contained inferences the participant was assigned to validate (a maintenance-covered
participant is still selected for validation duty); (3) claim-time validation checking is enabled.
Whether a multi-epoch window can be scheduled at all depends on Bug 2 — with a correct overlap check
it effectively cannot be, so Bug 1 is normally dormant.

Suggested fix: align the claim-side exemption with the credit-grant side by extracting the "is this
epoch covered by the participant's maintenance window?" check into a shared helper used by both call
sites. Residual limitation: the maintenance state tracks only the most recent window, so an
out-of-order claim for an epoch covered by an earlier window would still not be exempted; resolving
that fully would require per-epoch tracking.

## Bug 2 — phase-overlap check examines only a fixed number of upcoming epochs

When a window is scheduled, the code checks whether it overlaps any epoch's critical phases (PoC
commit/exchange, PoC validation, DKG). That check examines only the current epoch plus a fixed five
upcoming epochs. If the window extends past that horizon, the portion beyond it is not examined: a
window positioned entirely within the free regions of the examined epochs but whose tail falls in
the busy region of an unexamined epoch is incorrectly accepted.

The comment on the fixed limit asserts it is "sufficient because the maximum maintenance window
duration is governance-capped well below 5 full epoch lengths", but nothing enforces that: the
maximum window duration is a governance parameter whose only validation is an overflow guard set to
a very large constant. The window start height is also unbounded, so a window scheduled far in
advance can likewise move its covered epochs past the horizon.

Precondition: a window long enough, and positioned such, that it overlaps a critical phase in an
epoch beyond the examination horizon.

Suggested fix: do not use a fixed lookahead. Either extend the examined range until it passes the
window's end height (with a hard cap retained purely for DoS protection), or enforce in parameter
validation that the maximum window duration is below N epoch lengths so that a fixed lookahead is
genuinely sufficient. Applying both measures is recommended.

## Bug 3 — `maintenance_enabled` disabled mid-window leaves the reservation stuck in ACTIVE

`ProcessMaintenanceTransitions` runs every block and drives the reservation lifecycle (ACTIVE at the
start height, COMPLETED at the end height). It begins by checking `MaintenanceEnabled` and returns
early if the feature is disabled, processing no transitions at all. Consequently, if governance
disables the feature while a window is active, that window's COMPLETE transition is never processed:
transition records are keyed by exact height, and once that height has passed it is never revisited,
even if the feature is later re-enabled. The reservation remains in the ACTIVE state indefinitely.

Furthermore, the "is this participant in maintenance?" check does not consult `MaintenanceEnabled`,
and the slashing path relies on it to exempt downtime penalties; the stuck participant is therefore
permanently immune to jailing and slashing for downtime.

Related inconsistency: the exemption paths treat `MaintenanceEnabled` inconsistently — some stop
exempting as soon as the feature is disabled (inference assignment, credit accrual), while others do
not consult the flag and continue exempting (slashing, the inference-timeout penalty, CPoC duty). A
disabled feature with a window still active therefore leaves a partially-disabled state. This
surfaces only under the Bug 3 precondition.

Precondition: governance sets `maintenance_enabled` to false while a reservation is active (for
example, an emergency disable after a defect is discovered, or a parameter rollback).

Suggested fix: define `MaintenanceEnabled = false` to mean "no new windows are scheduled or
activated, but in-flight windows wind down normally" — continue processing COMPLETE transitions when
the feature is disabled, gating only new ACTIVATE transitions — and make the other exemption paths
follow the reservation state machine rather than the flag. The test
`TestLifecycle_ActiveReservationCompletesWhenMaintenanceDisabled` encodes the core expectation: with
the feature disabled, COMPLETE still fires at the scheduled height, the reservation becomes
COMPLETED, and the maintenance state is cleared.

## Relationship between the bugs

- Bug 2 is the practical trigger for Bug 1: Bug 1 manifests only if a multi-epoch window has been
  activated, and in production the only realistic way to reach that state is via Bug 2's gap in the
  overlap check (the epoch boundary lies within a busy region, so a correct check rejects every
  cross-epoch window). Fixing Bug 2 alone renders Bug 1 effectively unreachable in practice. Bug 1
  nonetheless remains a genuine, independent inconsistency: other paths — for example, a governance
  change to the epoch length — can re-establish the precondition, and the codebase already provides
  handling for multi-epoch windows. Both should therefore be fixed (defense in depth).
- Bug 3 is largely independent of Bugs 1 and 2 — it is triggered by a governance flip, not by
  cross-epoch windows. Its related inconsistency in flag handling should be addressed together with
  Bug 3, so that disabling the feature means only "stop new windows".

## What the proposal says (and does not)

The proposal does not prohibit cross-epoch windows; the only epoch-related constraints are that a
window must not overlap the PoC commit/exchange window or the DKG window. Its credit model explicitly
specifies suppressing credit for "any epoch in which a maintenance window was activated", which
presumes that multi-epoch windows are possible and must be handled. It also scopes `maintenance_enabled`
to "enables or disables scheduling and activation of maintenance windows" — that is, new windows
only, which aligns with the suggested fix for Bug 3.

## Tests added (committed alongside this document; all currently failing)

- `TestMsgServer_ClaimRewards_MaintenanceWindowExemptsValidationAcrossEpochRange` — simulates a
  window covering epochs 5 through 7 and claims epoch 6's rewards (a mid epoch), expecting the claim
  to succeed.
- `TestScheduleMaintenance_RejectsPoCOverlapBeyondFixedFutureEpochScan` — schedules a long window
  whose tail falls in the busy region of an epoch beyond the overlap check's horizon, expecting it to
  be rejected.
- `TestLifecycle_ActiveReservationCompletesWhenMaintenanceDisabled` — activates a window, disables
  the feature mid-window, advances to the end height, expecting the reservation to become COMPLETED
  and the maintenance state to be cleared.

All three should pass once the corresponding fixes are applied.
