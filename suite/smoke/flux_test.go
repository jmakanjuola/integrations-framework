package smoke

//revive:disable:dot-imports
import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	uuid "github.com/satori/go.uuid"
	"github.com/smartcontractkit/helmenv/environment"
	"github.com/smartcontractkit/helmenv/tools"
	"github.com/smartcontractkit/integrations-framework/actions"
	"github.com/smartcontractkit/integrations-framework/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rs/zerolog/log"
	"github.com/smartcontractkit/integrations-framework/client"
	"github.com/smartcontractkit/integrations-framework/contracts"
)

var _ = Describe("Flux monitor suite @flux", func() {
	var (
		err              error
		nets             *client.Networks
		cd               contracts.ContractDeployer
		lt               contracts.LinkToken
		fluxInstance     contracts.FluxAggregator
		cls              []client.Chainlink
		mockserver       *client.MockserverClient
		nodeAddresses    []common.Address
		adapterPath      string
		adapterUUID      string
		fluxRoundTimeout = 2 * time.Minute
		e                *environment.Environment
	)
	BeforeEach(func() {
		By("Deploying the environment", func() {
			e, err = environment.DeployOrLoadEnvironment(
				environment.NewChainlinkConfig(environment.ChainlinkReplicas(3, nil)),
				tools.ChartsRoot,
			)
			Expect(err).ShouldNot(HaveOccurred(), "Environment deployment shouldn't fail")
			err = e.ConnectAll()
			Expect(err).ShouldNot(HaveOccurred(), "Connecting to all nodes shouldn't fail")
		})

		By("Connecting to launched resources", func() {
			networkRegistry := client.NewNetworkRegistry()
			nets, err = networkRegistry.GetNetworks(e)
			Expect(err).ShouldNot(HaveOccurred(), "Connecting to blockchain nodes shouldn't fail")
			cd, err = contracts.NewContractDeployer(nets.Default)
			Expect(err).ShouldNot(HaveOccurred(), "Deploying contracts shouldn't fail")
			cls, err = client.ConnectChainlinkNodes(e)
			Expect(err).ShouldNot(HaveOccurred(), "Connecting to chainlink nodes shouldn't fail")
			nodeAddresses, err = actions.ChainlinkNodeAddresses(cls)
			Expect(err).ShouldNot(HaveOccurred(), "Retreiving on-chain wallet addresses for chainlink nodes shouldn't fail")
			mockserver, err = client.ConnectMockServer(e)
			Expect(err).ShouldNot(HaveOccurred(), "Creating mock server client shouldn't fail")
			nets.Default.ParallelTransactions(true)
		})

		By("Setting initial adapter value", func() {
			adapterUUID = uuid.NewV4().String()
			adapterPath = fmt.Sprintf("/variable-%s", adapterUUID)
			err = mockserver.SetValuePath(adapterPath, 1e5)
			Expect(err).ShouldNot(HaveOccurred(), "Setting mockserver value path shouldn't fail")
		})

		By("Deploying and funding contract", func() {
			lt, err = cd.DeployLinkTokenContract()
			Expect(err).ShouldNot(HaveOccurred(), "Deploying Link Token Contract shouldn't fail")
			fluxInstance, err = cd.DeployFluxAggregatorContract(lt.Address(), contracts.DefaultFluxAggregatorOptions())
			Expect(err).ShouldNot(HaveOccurred(), "Deploying Flux Aggregator Contract shouldn't fail")
			err = lt.Transfer(fluxInstance.Address(), big.NewInt(1e18))
			Expect(err).ShouldNot(HaveOccurred(), "Funding Flux Aggregator Contract shouldn't fail")
			err = fluxInstance.UpdateAvailableFunds()
			Expect(err).ShouldNot(HaveOccurred(), "Getting the available funds on the Flux Aggragator Contract shouldn't fail")
			err = nets.Default.WaitForEvents()
			Expect(err).ShouldNot(HaveOccurred(), "Waiting for event subscriptions in nodes shouldn't fail")
		})

		By("Funding Chainlink nodes", func() {
			err = actions.FundChainlinkNodes(cls, nets.Default, big.NewFloat(1))
			Expect(err).ShouldNot(HaveOccurred(), "Funding chainlink nodes with ETH shouldn't fail")
		})

		By("Setting oracle options", func() {
			err = fluxInstance.SetOracles(
				contracts.FluxAggregatorSetOraclesOptions{
					AddList:            nodeAddresses,
					RemoveList:         []common.Address{},
					AdminList:          nodeAddresses,
					MinSubmissions:     3,
					MaxSubmissions:     3,
					RestartDelayRounds: 0,
				})
			Expect(err).ShouldNot(HaveOccurred(), "Setting oracle options in the Flux Aggregator contract shouldn't fail")
			err = nets.Default.WaitForEvents()
			Expect(err).ShouldNot(HaveOccurred(), "Waiting for event subscriptions in nodes shouldn't fail")
			oracles, err := fluxInstance.GetOracles(context.Background())
			Expect(err).ShouldNot(HaveOccurred(), "Getting oracle details from the Flux aggregator contract shouldn't fail")
			log.Info().Str("Oracles", strings.Join(oracles, ",")).Msg("Oracles set")
		})

		By("Creating flux jobs", func() {
			adapterFullURL := fmt.Sprintf("%s%s", mockserver.Config.ClusterURL, adapterPath)
			bta := client.BridgeTypeAttributes{
				Name: fmt.Sprintf("variable-%s", adapterUUID),
				URL:  adapterFullURL,
			}
			for i, n := range cls {
				err = n.CreateBridge(&bta)
				Expect(err).ShouldNot(HaveOccurred(), "Creating bridge shouldn't fail for node %d", i+1)

				fluxSpec := &client.FluxMonitorJobSpec{
					Name:              fmt.Sprintf("flux-monitor-%s", adapterUUID),
					ContractAddress:   fluxInstance.Address(),
					Threshold:         0,
					AbsoluteThreshold: 0,
					PollTimerPeriod:   15 * time.Second, // min 15s
					IdleTimerDisabled: true,
					ObservationSource: client.ObservationSourceSpecBridge(bta),
				}
				_, err = n.CreateJob(fluxSpec)
				Expect(err).ShouldNot(HaveOccurred(), "Creating flux job shouldn't fail for node %d", i+1)
			}
		})
	})

	Describe("with Flux job", func() {
		It("performs two rounds and has withdrawable payments for oracles", func() {
			// initial value set is performed before jobs creation
			fluxRound := contracts.NewFluxAggregatorRoundConfirmer(fluxInstance, big.NewInt(1), fluxRoundTimeout)
			nets.Default.AddHeaderEventSubscription(fluxInstance.Address(), fluxRound)
			err = nets.Default.WaitForEvents()
			Expect(err).ShouldNot(HaveOccurred(), "Waiting for event subscriptions in nodes shouldn't fail")
			data, err := fluxInstance.GetContractData(context.Background())
			Expect(err).ShouldNot(HaveOccurred(), "Getting contract data from flux aggregator contract shouldn't fail")
			log.Info().Interface("Data", data).Msg("Round data")
			Expect(data.LatestRoundData.Answer.Int64()).Should(Equal(int64(1e5)), "Expected latest round answer to be %d, but found %d", int64(1e5), data.LatestRoundData.Answer.Int64())
			Expect(data.LatestRoundData.RoundId.Int64()).Should(Equal(int64(1)), "Expected latest round id to be %d, but found %d", int64(1), data.LatestRoundData.RoundId.Int64())
			Expect(data.LatestRoundData.AnsweredInRound.Int64()).Should(Equal(int64(1)), "Expected latest round's answered in round to be %d, but found %d", int64(1), data.LatestRoundData.AnsweredInRound.Int64())
			Expect(data.AvailableFunds.Int64()).Should(Equal(int64(999999999999999997)), "Expected available funds to be %d, but found %d", int64(999999999999999997), data.AvailableFunds.Int64())
			Expect(data.AllocatedFunds.Int64()).Should(Equal(int64(3)), "Expected allocated funds to be %d, but found %d", int64(3), data.AllocatedFunds.Int64())

			fluxRound = contracts.NewFluxAggregatorRoundConfirmer(fluxInstance, big.NewInt(2), fluxRoundTimeout)
			nets.Default.AddHeaderEventSubscription(fluxInstance.Address(), fluxRound)
			err = mockserver.SetValuePath(adapterPath, 1e10)
			Expect(err).ShouldNot(HaveOccurred(), "Setting value path in mock server shoudln't fail")
			err = nets.Default.WaitForEvents()
			Expect(err).ShouldNot(HaveOccurred(), "Waiting for event subscriptions in nodes shouldn't fail")
			data, err = fluxInstance.GetContractData(context.Background())
			Expect(err).ShouldNot(HaveOccurred(), "Getting contract data from flux aggregator contract shouldn't fail")
			Expect(data.LatestRoundData.Answer.Int64()).Should(Equal(int64(1e10)), "Expected latest round answer to be %d, but found %d", int64(1e10), data.LatestRoundData.Answer.Int64())
			Expect(data.LatestRoundData.RoundId.Int64()).Should(Equal(int64(2)), "Expected latest round id to be %d, but found %d", int64(2), data.LatestRoundData.RoundId.Int64())
			Expect(data.LatestRoundData.AnsweredInRound.Int64()).Should(Equal(int64(2)), "Expected latest round's answered in round to be %d, but found %d", int64(2), data.LatestRoundData.AnsweredInRound.Int64())
			Expect(data.AvailableFunds.Int64()).Should(Equal(int64(999999999999999994)), "Expected available funds to be %d, but found %d", int64(999999999999999994), data.AvailableFunds.Int64())
			Expect(data.AllocatedFunds.Int64()).Should(Equal(int64(6)), "Expected allocated funds to be %d, but found %d", int64(6), data.AllocatedFunds.Int64())
			log.Info().Interface("data", data).Msg("Round data")

			for _, oracleAddr := range nodeAddresses {
				payment, _ := fluxInstance.WithdrawablePayment(context.Background(), oracleAddr)
				Expect(payment.Int64()).Should(Equal(int64(2)), "Expected flux aggregator contract's withdrawable payment to be %d, but found %d", int64(2), payment.Int64())
			}
		})
	})

	AfterEach(func() {
		By("Printing gas stats", func() {
			nets.Default.GasStats().PrintStats()
		})
		By("Tearing down the environment", func() {
			err = actions.TeardownSuite(e, nets, utils.ProjectRoot, nil)
			Expect(err).ShouldNot(HaveOccurred(), "Environment teardown shouldn't fail")
		})
	})
})
