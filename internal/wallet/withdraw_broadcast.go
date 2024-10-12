package wallet

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/goatnetwork/goat-relayer/internal/config"
	"github.com/goatnetwork/goat-relayer/internal/types"
	log "github.com/sirupsen/logrus"
)

func (w *WalletServer) withdrawProcessLoop(ctx context.Context) {
	log.Debug("WalletServer withdrawProcessLoop")
	// init status process, if restart && layer2 status is up to date, remove all status "create", "aggregating"
	if !w.state.GetBtcHead().Syncing {
		w.cleanWithdrawProcess()
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.execWithdrawSig()
		}
	}
}

func (w *WalletServer) execWithdrawSig() {
	l2Info := w.state.GetL2Info()
	if l2Info.Syncing {
		log.Infof("WalletServer execWithdrawSig ignore, layer2 is catching up")
		return
	}

	btcState := w.state.GetBtcHead()
	if btcState.Syncing {
		log.Infof("WalletServer execWithdrawSig ignore, btc is catching up")
		return
	}
	if btcState.NetworkFee > 500 {
		log.Infof("WalletServer execWithdrawSig ignore, btc network fee too high: %d", btcState.NetworkFee)
		return
	}

	w.txBroadcastMu.Lock()
	defer w.txBroadcastMu.Unlock()

	epochVoter := w.state.GetEpochVoter()
	if epochVoter.Proposer != config.AppConfig.RelayerAddress {
		// do not clean immediately
		if w.txBroadcastStatus && l2Info.Height > epochVoter.Height+1 {
			w.txBroadcastStatus = false
			// clean process, role changed, remove all status "create", "aggregating"
			w.cleanWithdrawProcess()
		}
		log.Debugf("WalletServer execWithdrawSig ignore, self is not proposer, epoch: %d, proposer: %s", epochVoter.Epoch, epochVoter.Proposer)
		return
	}

	// 2. check if there is a sig in progress
	if w.txBroadcastStatus {
		log.Debug("WalletServer execWithdrawSig ignore, there is a sig")
		return
	}
	if l2Info.LatestBtcHeight <= w.txBroadcastFinishBtcHeight+1 {
		log.Debugf("WalletServer execWithdrawSig ignore, last finish broadcast in this block: %d", w.txBroadcastFinishBtcHeight)
		return
	}

	sendOrders, err := w.state.GetSendOrderInitlized()
	if err != nil {
		log.Errorf("WalletServer execWithdrawSig error: %v", err)
		return
	}
	if len(sendOrders) == 0 {
		log.Debug("WalletServer execWithdrawSig ignore, no withdraw for sign")
		return
	}

	privKeyBytes, err := hex.DecodeString(config.AppConfig.FireblocksPrivKey)
	if err != nil {
		log.Errorf("WalletServer execWithdrawSig decode privKey error: %v", err)
	}
	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes)

	for _, sendOrder := range sendOrders {
		tx, err := types.DeserializeTransaction(sendOrder.NoWitnessTx)
		if err != nil {
			log.Errorf("WalletServer execWithdrawSig deserialize tx error: %v", err)
			return
		}

		utxos, err := w.state.GetUtxoByOrderId(sendOrder.OrderId)
		if err != nil {
			log.Errorf("WalletServer execWithdrawSig get utxos error: %v", err)
			continue
		}

		// sign the transaction
		err = SignTransactionByPrivKey(privKey, tx, utxos, types.GetBTCNetwork(config.AppConfig.BTCNetworkType))
		if err != nil {
			log.Errorf("WalletServer execWithdrawSig sign tx error: %v", err)
			continue
		}

		// broadcast the transaction
		txHash, err := w.btcClient.SendRawTransaction(tx, false)
		if err != nil {
			log.Errorf("WalletServer execWithdrawSig broadcast tx error: %v", err)
			continue
		}

		// update sendOrder status to pending
		err = w.state.UpdateSendOrderPending(txHash.String())
		if err != nil {
			log.Errorf("WalletServer execWithdrawSig update sendOrder status error: %v", err)
			continue
		}
	}

}
