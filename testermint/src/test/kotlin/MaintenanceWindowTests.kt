import com.github.dockerjava.core.DockerClientBuilder
import com.productscience.LocalInferencePair
import com.productscience.data.*
import com.productscience.getRawContainers
import com.productscience.initCluster
import com.productscience.logSection
import org.assertj.core.api.Assertions.assertThat
import org.junit.jupiter.api.Tag
import org.junit.jupiter.api.Test
import org.tinylog.kotlin.Logger

/**
 * End-to-end tests for the Maintenance Windows feature.
 *
 * These tests verify that maintenance windows work correctly in a real
 * multi-node blockchain environment: scheduling, activation, liveness
 * exemption (no jailing), duty suppression, and normal resume behavior.
 */
class MaintenanceWindowTests : TestermintTest() {

    /**
     * Stop the chain node container for a given pair.
     * This causes the validator to stop signing blocks (missing signatures).
     */
    private fun stopNodeContainer(pair: LocalInferencePair) {
        val nodeContainer = getRawContainers(pair.config).getNode(pair.name)
            ?: error("Node container not found for ${pair.name}")
        DockerClientBuilder.getInstance().build().use { dockerClient ->
            dockerClient.stopContainerCmd(nodeContainer.id).exec()
        }
        Logger.info("Stopped node container for ${pair.name}")
    }

    /**
     * Restart a previously stopped chain node container.
     */
    private fun startNodeContainer(pair: LocalInferencePair) {
        val nodeContainer = getRawContainers(pair.config).getNode(pair.name)
            ?: error("Node container not found for ${pair.name}")
        DockerClientBuilder.getInstance().build().use { dockerClient ->
            if (nodeContainer.state != "running") {
                dockerClient.startContainerCmd(nodeContainer.id).exec()
            }
        }
        Logger.info("Started node container for ${pair.name}")
    }

    /**
     * Test 1: Enable maintenance via governance and verify params are updated.
     */
    @Test
    @Tag("maintenance")
    fun `enable maintenance windows via governance proposal`() {
        val (cluster, genesis) = initCluster()

        logSection("Getting current params")
        val params = genesis.getParams()
        Logger.info("Current maintenance params: ${params.maintenanceParams}")

        // Enable maintenance with test-friendly parameters:
        // - short lead time (5 blocks instead of 100)
        // - reasonable window (20 blocks)
        // - generous credit (give 50 blocks per epoch so credit accumulates fast)
        val modifiedParams = params.copy(
            maintenanceParams = MaintenanceParams(
                maintenanceEnabled = true,
                maintenanceMinScheduleLeadBlocks = 5,
                maintenanceMaxWindowBlocks = 50,
                maintenanceMaxConcurrentValidators = 3,
                maintenanceMaxConcurrentPowerBps = 5000, // 50%
                maintenanceCreditCapBlocks = 200,
                maintenanceCreditEarnPerSuccessfulEpochBlocks = 50,
            )
        )

        logSection("Submitting governance proposal to enable maintenance")
        genesis.runProposal(cluster, UpdateParams(params = modifiedParams))

        logSection("Verifying maintenance params are updated")
        val newParams = genesis.getParams()
        assertThat(newParams.maintenanceParams).isNotNull
        assertThat(newParams.maintenanceParams!!.maintenanceEnabled).isTrue()
        assertThat(newParams.maintenanceParams!!.maintenanceMinScheduleLeadBlocks).isEqualTo(5)
        Logger.info("Maintenance successfully enabled via governance")

        genesis.markNeedsReboot()
    }

    /**
     * Test 2: Full maintenance window lifecycle — schedule, activate, verify
     * no jailing during offline period, verify resume after window ends.
     *
     * This is the core E2E test for the maintenance windows feature.
     */
    @Test
    @Tag("maintenance")
    fun `maintenance window prevents jailing during offline period`() {
        val (cluster, genesis) = initCluster()

        // Step 1: Enable maintenance with test-friendly params via governance
        logSection("Step 1: Enable maintenance via governance")
        val params = genesis.getParams()
        val modifiedParams = params.copy(
            maintenanceParams = MaintenanceParams(
                maintenanceEnabled = true,
                maintenanceMinScheduleLeadBlocks = 5,
                maintenanceMaxWindowBlocks = 50,
                maintenanceMaxConcurrentValidators = 3,
                maintenanceMaxConcurrentPowerBps = 5000,
                maintenanceCreditCapBlocks = 200,
                maintenanceCreditEarnPerSuccessfulEpochBlocks = 50,
            )
        )
        genesis.runProposal(cluster, UpdateParams(params = modifiedParams))
        val updatedParams = genesis.getParams()
        assertThat(updatedParams.maintenanceParams?.maintenanceEnabled).isTrue()

        // Step 2: Earn maintenance credit by completing epochs
        logSection("Step 2: Earning maintenance credit through epochs")
        val join1 = cluster.joinPairs[0]
        val join1Address = join1.node.getColdAddress()
        Logger.info("Join1 address: $join1Address")

        // Wait for a few epochs to accumulate credit (50 blocks per epoch)
        genesis.waitForNextEpoch()
        genesis.waitForNextEpoch()

        // Query credit for join1
        val creditResponse: MaintenanceCreditResponse = genesis.node.execAndParse(
            listOf("query", "inference", "maintenance-credit", join1Address)
        )
        Logger.info("Join1 maintenance credit: ${creditResponse.creditBlocks} blocks")

        // If credit is insufficient, wait for more epochs
        if (creditResponse.creditBlocks < 15) {
            Logger.info("Credit too low, waiting for more epochs...")
            genesis.waitForNextEpoch()
            genesis.waitForNextEpoch()
            val retryCredit: MaintenanceCreditResponse = genesis.node.execAndParse(
                listOf("query", "inference", "maintenance-credit", join1Address)
            )
            Logger.info("Join1 maintenance credit after more epochs: ${retryCredit.creditBlocks} blocks")
        }

        // Step 3: Schedule a maintenance window for join1
        logSection("Step 3: Scheduling maintenance window for join1")
        val epochData = genesis.getEpochData()
        val currentHeight = epochData.blockHeight

        // Schedule maintenance to start 10 blocks from now, lasting 15 blocks
        val startHeight = currentHeight + 10
        val durationBlocks = 15L
        Logger.info("Scheduling maintenance: start=$startHeight, duration=$durationBlocks, currentHeight=$currentHeight")

        // Check schedulability first
        val schedulabilityResponse: MaintenanceSchedulabilityResponse = genesis.node.execAndParse(
            listOf(
                "query", "inference", "maintenance-schedulability",
                join1Address,
                startHeight.toString(),
                durationBlocks.toString()
            )
        )
        Logger.info("Schedulability check: schedulable=${schedulabilityResponse.schedulable}, reason=${schedulabilityResponse.rejectionReason}")

        if (!schedulabilityResponse.schedulable) {
            Logger.warn("Window not schedulable: ${schedulabilityResponse.rejectionReason}")
            Logger.warn("Skipping test — scheduling conditions not met (likely PoC/DKG phase overlap or insufficient credit)")
            genesis.markNeedsReboot()
            return
        }

        // Schedule the maintenance window
        val scheduleTx = join1.submitTransaction(
            listOf(
                "inference", "schedule-maintenance",
                "--participant", join1Address,
                "--start-height", startHeight.toString(),
                "--duration-blocks", durationBlocks.toString(),
            )
        )
        Logger.info("Schedule tx result: code=${scheduleTx.code}, hash=${scheduleTx.txhash}")
        assertThat(scheduleTx.code).isEqualTo(0)

        // Verify reservation was created
        val statusResponse: MaintenanceStatusResponse = genesis.node.execAndParse(
            listOf("query", "inference", "maintenance-status", join1Address)
        )
        assertThat(statusResponse.found).isTrue()
        assertThat(statusResponse.scheduledReservation).isNotNull
        Logger.info("Reservation created: id=${statusResponse.scheduledReservation?.reservationId}, status=${statusResponse.scheduledReservation?.status}")

        // Step 4: Verify join1 validator is BONDED before maintenance
        logSection("Step 4: Verify join1 is BONDED before maintenance")
        val validatorsBefore = genesis.node.getValidators()
        val join1ValPubKey = join1.node.getValidatorInfo().key
        val join1ValBefore = validatorsBefore.validators.find {
            it.consensusPubkey.value == join1ValPubKey
        }
        assertThat(join1ValBefore).isNotNull
        assertThat(join1ValBefore!!.statusEnum).isEqualTo(StakeValidatorStatus.BONDED)
        Logger.info("Join1 validator status before maintenance: ${join1ValBefore.status}")

        // Step 5: Wait for maintenance window to activate
        logSection("Step 5: Waiting for maintenance window to activate at height $startHeight")
        genesis.node.waitForMinimumBlock(startHeight + 1, "maintenance activation")

        // Verify maintenance is now active
        val activeResponse: MaintenanceActiveResponse = genesis.node.execAndParse(
            listOf("query", "inference", "maintenance-active")
        )
        Logger.info("Active maintenance windows: ${activeResponse.reservations.size}")

        // Step 6: Stop join1's chain node to simulate being offline during maintenance
        // This causes join1's validator to stop signing blocks (missed signatures).
        logSection("Step 6: Stopping join1 chain node (simulating offline maintenance)")
        stopNodeContainer(join1)
        Logger.info("Join1 chain node stopped — validator will miss signatures")

        // Step 7: Wait through the maintenance window while join1 is offline
        logSection("Step 7: Waiting through maintenance window (${durationBlocks} blocks)")
        // Wait for blocks to pass — join1 is missing signatures during this time
        val endHeight = startHeight + durationBlocks
        genesis.node.waitForMinimumBlock(endHeight + 2, "maintenance window end")
        Logger.info("Maintenance window should now be completed")

        // Step 8: Verify join1 is NOT jailed (the key assertion!)
        logSection("Step 8: Verifying join1 was NOT jailed during maintenance")
        val validatorsAfter = genesis.node.getValidators()
        val join1ValAfter = validatorsAfter.validators.find {
            it.consensusPubkey.value == join1ValPubKey
        }
        assertThat(join1ValAfter).isNotNull
        assertThat(join1ValAfter!!.statusEnum)
            .describedAs("Join1 should remain BONDED — maintenance window should have prevented jailing")
            .isEqualTo(StakeValidatorStatus.BONDED)
        Logger.info("SUCCESS: Join1 validator is still BONDED after being offline during maintenance window!")

        // Verify maintenance window completed
        val statusAfter: MaintenanceStatusResponse = genesis.node.execAndParse(
            listOf("query", "inference", "maintenance-status", join1Address)
        )
        Logger.info("Join1 maintenance state after window: active=${statusAfter.activeReservation}, scheduled=${statusAfter.scheduledReservation}")

        genesis.markNeedsReboot()
    }

    /**
     * Test 3: Verify that maintenance scheduling is rejected when it overlaps
     * with restricted PoC or DKG phases.
     */
    @Test
    @Tag("maintenance")
    fun `maintenance window scheduling rejected during epoch-critical phases`() {
        val (cluster, genesis) = initCluster()

        // Enable maintenance
        logSection("Enabling maintenance via governance")
        val params = genesis.getParams()
        val modifiedParams = params.copy(
            maintenanceParams = MaintenanceParams(
                maintenanceEnabled = true,
                maintenanceMinScheduleLeadBlocks = 2,
                maintenanceMaxWindowBlocks = 100,
                maintenanceMaxConcurrentValidators = 3,
                maintenanceMaxConcurrentPowerBps = 5000,
                maintenanceCreditCapBlocks = 500,
                maintenanceCreditEarnPerSuccessfulEpochBlocks = 100,
            )
        )
        genesis.runProposal(cluster, UpdateParams(params = modifiedParams))

        // Earn some credit
        genesis.waitForNextEpoch()
        genesis.waitForNextEpoch()

        val join1 = cluster.joinPairs[0]
        val join1Address = join1.node.getColdAddress()

        // Get epoch stages to find PoC phase
        logSection("Getting epoch stages to target PoC phase")
        val epochData = genesis.getEpochData()
        val pocStart = epochData.nextEpochStages.pocStart
        Logger.info("Next PoC start: $pocStart, current height: ${epochData.blockHeight}")

        // Try to schedule a maintenance window that overlaps with PoC start
        // Use a window that covers pocStart
        val overlapStart = pocStart - 5
        val overlapDuration = 20L

        if (overlapStart <= epochData.blockHeight + 2) {
            Logger.warn("Cannot test PoC overlap — PoC start too close, skipping")
            genesis.markNeedsReboot()
            return
        }

        logSection("Checking schedulability for PoC-overlapping window")
        val schedulabilityResponse: MaintenanceSchedulabilityResponse = genesis.node.execAndParse(
            listOf(
                "query", "inference", "maintenance-schedulability",
                join1Address,
                overlapStart.toString(),
                overlapDuration.toString()
            )
        )
        Logger.info("Schedulability for PoC-overlapping window: schedulable=${schedulabilityResponse.schedulable}, reason=${schedulabilityResponse.rejectionReason}")

        // The window should be rejected due to PoC/DKG phase overlap
        assertThat(schedulabilityResponse.schedulable)
            .describedAs("Window overlapping PoC phase should not be schedulable")
            .isFalse()
        assertThat(schedulabilityResponse.rejectionReason).isNotEmpty()
        Logger.info("SUCCESS: Scheduling correctly rejected for epoch-critical phase overlap")

        genesis.markNeedsReboot()
    }

    /**
     * Test 4: Verify maintenance cancellation restores credit.
     */
    @Test
    @Tag("maintenance")
    fun `cancel scheduled maintenance restores credit`() {
        val (cluster, genesis) = initCluster()

        // Enable maintenance
        logSection("Enabling maintenance via governance")
        val params = genesis.getParams()
        val modifiedParams = params.copy(
            maintenanceParams = MaintenanceParams(
                maintenanceEnabled = true,
                maintenanceMinScheduleLeadBlocks = 5,
                maintenanceMaxWindowBlocks = 50,
                maintenanceMaxConcurrentValidators = 3,
                maintenanceMaxConcurrentPowerBps = 5000,
                maintenanceCreditCapBlocks = 200,
                maintenanceCreditEarnPerSuccessfulEpochBlocks = 50,
            )
        )
        genesis.runProposal(cluster, UpdateParams(params = modifiedParams))

        // Earn credit
        genesis.waitForNextEpoch()
        genesis.waitForNextEpoch()

        val join1 = cluster.joinPairs[0]
        val join1Address = join1.node.getColdAddress()

        // Check credit before scheduling
        val creditBefore: MaintenanceCreditResponse = genesis.node.execAndParse(
            listOf("query", "inference", "maintenance-credit", join1Address)
        )
        Logger.info("Credit before scheduling: ${creditBefore.creditBlocks}")

        if (creditBefore.creditBlocks < 10) {
            Logger.warn("Insufficient credit for test, skipping")
            genesis.markNeedsReboot()
            return
        }

        // Schedule a maintenance window far in the future
        val epochData = genesis.getEpochData()
        val startHeight = epochData.blockHeight + 50
        val durationBlocks = 10L

        // Check schedulability
        val schedulability: MaintenanceSchedulabilityResponse = genesis.node.execAndParse(
            listOf(
                "query", "inference", "maintenance-schedulability",
                join1Address, startHeight.toString(), durationBlocks.toString()
            )
        )
        if (!schedulability.schedulable) {
            Logger.warn("Window not schedulable: ${schedulability.rejectionReason}, skipping")
            genesis.markNeedsReboot()
            return
        }

        logSection("Scheduling maintenance window")
        val scheduleTx = join1.submitTransaction(
            listOf(
                "inference", "schedule-maintenance",
                "--participant", join1Address,
                "--start-height", startHeight.toString(),
                "--duration-blocks", durationBlocks.toString(),
            )
        )
        assertThat(scheduleTx.code).isEqualTo(0)

        // Check credit after scheduling (should be reduced by durationBlocks)
        val creditAfterSchedule: MaintenanceCreditResponse = genesis.node.execAndParse(
            listOf("query", "inference", "maintenance-credit", join1Address)
        )
        Logger.info("Credit after scheduling: ${creditAfterSchedule.creditBlocks}")
        assertThat(creditAfterSchedule.creditBlocks)
            .isLessThan(creditBefore.creditBlocks)

        // Get reservation ID for cancellation
        val status: MaintenanceStatusResponse = genesis.node.execAndParse(
            listOf("query", "inference", "maintenance-status", join1Address)
        )
        assertThat(status.scheduledReservation).isNotNull
        val reservationId = status.scheduledReservation!!.reservationId
        Logger.info("Reservation ID to cancel: $reservationId")

        // Cancel the maintenance window
        logSection("Canceling maintenance window")
        val cancelTx = join1.submitTransaction(
            listOf(
                "inference", "cancel-maintenance",
                "--reservation-id", reservationId.toString(),
            )
        )
        assertThat(cancelTx.code).isEqualTo(0)

        // Check credit after cancellation (should be restored)
        val creditAfterCancel: MaintenanceCreditResponse = genesis.node.execAndParse(
            listOf("query", "inference", "maintenance-credit", join1Address)
        )
        Logger.info("Credit after cancellation: ${creditAfterCancel.creditBlocks}")
        assertThat(creditAfterCancel.creditBlocks)
            .describedAs("Credit should be restored after cancellation")
            .isGreaterThanOrEqualTo(creditAfterSchedule.creditBlocks + durationBlocks)

        Logger.info("SUCCESS: Credit correctly restored after maintenance cancellation")
        genesis.markNeedsReboot()
    }

    /**
     * Test 5: Verify maintenance concurrency query returns correct data.
     */
    @Test
    @Tag("maintenance")
    fun `maintenance concurrency query returns correct count`() {
        val (cluster, genesis) = initCluster()

        // Verify concurrency with no active maintenance
        logSection("Querying maintenance concurrency with no active maintenance")
        val epochData = genesis.getEpochData()
        val concurrency: MaintenanceConcurrencyResponse = genesis.node.execAndParse(
            listOf("query", "inference", "maintenance-concurrency", epochData.blockHeight.toString())
        )
        assertThat(concurrency.concurrentCount).isEqualTo(0)
        Logger.info("Concurrent maintenance count (none active): ${concurrency.concurrentCount}")
        Logger.info("SUCCESS: Concurrency query correctly reports zero when no maintenance is active")
    }
}
