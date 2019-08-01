package beacon

import (
	"context"

	"github.com/ipfs/go-log"

	"github.com/keep-network/keep-core/pkg/beacon/relay"
	"github.com/keep-network/keep-core/pkg/beacon/relay/event"
	"github.com/keep-network/keep-core/pkg/beacon/relay/groupselection"
	"github.com/keep-network/keep-core/pkg/beacon/relay/registry"
	"github.com/keep-network/keep-core/pkg/chain"
	"github.com/keep-network/keep-core/pkg/net"
	"github.com/keep-network/keep-core/pkg/persistence"
)

var logger = log.Logger("keep-beacon")

// Initialize kicks off the random beacon by initializing internal state,
// ensuring preconditions like staking are met, and then kicking off the
// internal random beacon implementation. Returns an error if this failed,
// otherwise enters a blocked loop.
func Initialize(
	ctx context.Context,
	stakingID string,
	chainHandle chain.Handle,
	netProvider net.Provider,
	persistence persistence.Handle,
) error {
	relayChain := chainHandle.ThresholdRelay()
	chainConfig, err := relayChain.GetConfig()
	if err != nil {
		return err
	}

	stakeMonitor, err := chainHandle.StakeMonitor()
	if err != nil {
		return err
	}

	staker, err := stakeMonitor.StakerFor(stakingID)
	if err != nil {
		return err
	}

	blockCounter, err := chainHandle.BlockCounter()
	if err != nil {
		return err
	}

	signing := chainHandle.Signing()

	groupRegistry := registry.NewGroupRegistry(relayChain, persistence)
	groupRegistry.LoadExistingGroups()

	node := relay.NewNode(
		staker,
		netProvider,
		blockCounter,
		chainConfig,
		groupRegistry,
	)

	relayChain.OnSignatureRequested(func(request *event.Request) {
		logger.Infof("new relay entry requested: [%+v]", request)

		selectedParticipants, err := relayChain.GetSelectedParticipants()
		if err != nil {
			logger.Errorf("could not fetch selected participants after challenge timeout [%v]", err)
		}
		selectedStakers := make([][]byte, len(selectedParticipants))
		for i, participant := range selectedParticipants {
			selectedStakers[i] = []byte(participant)
		}

		if (node.IsSelectedForGroup(&groupselection.Result{SelectedStakers: selectedStakers})) {
			go node.GenerateRelayEntryIfEligible(
				request.PreviousEntry,
				request.Seed,
				relayChain,
				request.GroupPublicKey,
				request.BlockNumber,
			)
		} else {
			go func() {
				logger.Infof("chain is being observed by the staker ID: [%+v] for a relay entry", node.Staker.ID())

				timeoutWaiterChannel, err := blockCounter.BlockHeightWaiter(request.BlockNumber + chainConfig.RelayEntryTimeout)
				if err != nil {
					logger.Errorf("block height waiter failure [%v]", err)
				}

				onSubmittedResultChannel := make(chan *event.Entry)

				subscription, err := relayChain.OnSignatureSubmitted(
					func(event *event.Entry) {
						onSubmittedResultChannel <- event
					},
				)
				if err != nil {
					close(onSubmittedResultChannel)
					logger.Errorf("could not watch for a signature submission: [%v]", err)
					return
				}

				for {
					select {
					case <-timeoutWaiterChannel:
						subscription.Unsubscribe()

						relayChain.HandleRelayEntryTimeout()
					case <-onSubmittedResultChannel:
						return
					}
				}
			}()
		}
	})

	relayChain.OnGroupSelectionStarted(func(event *event.GroupSelectionStart) {
		logger.Infof("group selection started: [%+v]", event)

		go func() {
			err := node.SubmitTicketsForGroupSelection(
				relayChain,
				blockCounter,
				signing,
				event.NewEntry.Bytes(),
				event.Seed,
				event.BlockNumber,
			)
			if err != nil {
				logger.Errorf("Tickets submission failed: [%v]", err)
			}
		}()
	})

	relayChain.OnGroupRegistered(func(registration *event.GroupRegistration) {
		logger.Infof("new group registered on chain: [%+v]", registration)
		go groupRegistry.UnregisterStaleGroups()
	})

	return nil
}
