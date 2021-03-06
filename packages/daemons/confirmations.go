// MIT License
//
// Copyright (c) 2016-2018 GACHAIN
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package daemons

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/GACHAIN/go-gachain/packages/conf"
	"github.com/GACHAIN/go-gachain/packages/config/syspar"
	"github.com/GACHAIN/go-gachain/packages/consts"
	"github.com/GACHAIN/go-gachain/packages/converter"
	"github.com/GACHAIN/go-gachain/packages/model"
	"github.com/GACHAIN/go-gachain/packages/tcpserver"

	log "github.com/sirupsen/logrus"
)

var tick int

// Confirmations gets and checks blocks from nodes
// Getting amount of nodes, which has the same hash as we do
func Confirmations(ctx context.Context, d *daemon) error {

	// the first 2 minutes we sleep for 10 sec for blocks to be collected
	tick++

	d.sleepTime = 1 * time.Second
	if tick < 12 {
		d.sleepTime = 10 * time.Second
	}

	var startBlockID int64

	// check last blocks, but not more than 5
	confirmations := &model.Confirmation{}
	_, err := confirmations.GetGoodBlock(consts.MIN_CONFIRMED_NODES)
	if err != nil {
		d.logger.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("getting good block")
		return err
	}

	ConfirmedBlockID := confirmations.BlockID
	infoBlock := &model.InfoBlock{}
	_, err = infoBlock.Get()
	if err != nil {
		d.logger.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("getting info block")
		return err
	}
	LastBlockID := infoBlock.BlockID
	if LastBlockID == 0 {
		return nil
	}

	if LastBlockID-ConfirmedBlockID > 5 {
		startBlockID = ConfirmedBlockID + 1
		d.sleepTime = 10 * time.Second
		tick = 0 // reset the tick
	}
	if startBlockID == 0 {
		startBlockID = LastBlockID
	}
	d.logger.WithFields(log.Fields{"start_block_id": startBlockID, "last_block_id": LastBlockID}).Info("confirming blocks from to")

	for blockID := LastBlockID; blockID >= startBlockID; blockID-- {
		if ctx.Err() != nil {
			d.logger.WithFields(log.Fields{"type": consts.ContextError, "error": err}).Error("error in context")
			return ctx.Err()
		}

		block := model.Block{}
		_, err := block.Get(blockID)
		if err != nil {
			d.logger.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("getting block by ID")
			return err
		}
		hashStr := string(converter.BinToHex(block.Hash))
		d.logger.WithFields(log.Fields{"hash": hashStr}).Debug("checking hash")
		if len(hashStr) == 0 {
			d.logger.WithFields(log.Fields{"hash": hashStr, "type": consts.NotFound}).Debug("hash not found")
			continue
		}

		var hosts []string
		if conf.Config.TestMode {
			hosts = []string{"localhost"}
		} else {
			hosts = syspar.GetRemoteHosts()
		}

		ch := make(chan string)
		for i := 0; i < len(hosts); i++ {
			// NOTE: host should not use default port number
			host := hosts[i] + ":" + strconv.Itoa(consts.DEFAULT_TCP_PORT)
			d.logger.WithFields(log.Fields{"host": host, "block_id": blockID}).Debug("checking block id confirmed at node")
			go func() {
				IsReachable(host, blockID, ch, d.logger)
			}()
		}
		var answer string
		var st0, st1 int64
		for i := 0; i < len(hosts); i++ {
			answer = <-ch
			if answer == hashStr {
				st1++
			} else {
				st0++
			}
		}
		confirmation := &model.Confirmation{}
		_, err = confirmation.GetConfirmation(blockID)
		if err == nil {
			confirmation.BlockID = blockID
			confirmation.Good = int32(st1)
			confirmation.Bad = int32(st0)
			confirmation.Time = int32(time.Now().Unix())
			err = confirmation.Save()
			if err != nil {
				d.logger.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("saving confirmation")
				return err
			}
		} else {
			confirmation.BlockID = blockID
			confirmation.Good = int32(st1)
			confirmation.Bad = int32(st0)
			confirmation.Time = int32(time.Now().Unix())
			err = confirmation.Save()
			if err != nil {
				d.logger.WithFields(log.Fields{"type": consts.DBError, "error": err}).Error("saving confirmation")
				return err
			}
		}
		if blockID > startBlockID && st1 >= consts.MIN_CONFIRMED_NODES {
			break
		}
	}

	return nil

}

func checkConf(host string, blockID int64, logger *log.Entry) string {
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		logger.WithFields(log.Fields{"type": consts.ConnectionError, "error": err, "host": host, "block_id": blockID}).Debug("dialing to host")
		return "0"
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(consts.READ_TIMEOUT * time.Second))
	conn.SetWriteDeadline(time.Now().Add(consts.WRITE_TIMEOUT * time.Second))

	type confRequest struct {
		Type    uint16
		BlockID uint32
	}
	err = tcpserver.SendRequest(&confRequest{Type: 4, BlockID: uint32(blockID)}, conn)
	if err != nil {
		logger.WithFields(log.Fields{"type": consts.IOError, "error": err, "host": host, "block_id": blockID}).Error("sending confirmation request")
		return "0"
	}

	resp := &tcpserver.ConfirmResponse{}
	err = tcpserver.ReadRequest(resp, conn)
	if err != nil {
		logger.WithFields(log.Fields{"type": consts.IOError, "error": err, "host": host, "block_id": blockID}).Error("receiving confirmation response")
		return "0"
	}
	return string(converter.BinToHex(resp.Hash))
}

// IsReachable checks if there is blockID on the host
func IsReachable(host string, blockID int64, ch0 chan string, logger *log.Entry) {
	ch := make(chan string, 1)
	go func() {
		ch <- checkConf(host, blockID, logger)
	}()
	select {
	case reachable := <-ch:
		ch0 <- reachable
	case <-time.After(consts.WAIT_CONFIRMED_NODES * time.Second):
		ch0 <- "0"
	}
}
