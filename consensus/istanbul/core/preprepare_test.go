// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/consensus/istanbul"
)

func newTestPreprepare(v *istanbul.View) *istanbul.Preprepare {
	return &istanbul.Preprepare{
		View:     v,
		Proposal: newTestProposal(),
	}
}

func TestValidateRoundChangeCertificate(t *testing.T) {
	N := uint64(4) // replica 0 is the proposer, it will send messages to others
	F := uint64(1)
	sys := NewTestSystemWithBackend(N, F)
	testCases := []struct {
		certificate istanbul.RoundChangeCertificate
		expectedErr error
	}{
		{
			// Valid round change certificate without PREPARED certificate
			func() istanbul.RoundChangeCertificate {
				var roundChangeCertificate istanbul.RoundChangeCertificate
				view := istanbul.View{
					Round:    big.NewInt(0),
					Sequence: big.NewInt(1),
				}
				for _, backend := range sys.backends {
					rc := &istanbul.RoundChange{
						View:                &view,
						PreparedCertificate: istanbul.EmptyPreparedCertificate(),
					}

					payload, err := Encode(rc)
					if err != nil {
						t.Errorf("Failed to encode ROUND CHANGE: %v", err)
					}

					msg := istanbul.Message{
						Code:          istanbul.MsgRoundChange,
						Msg:           payload,
						Address:       backend.address,
						CommittedSeal: []byte{},
					}

					data, err := msg.PayloadNoSig()
					if err != nil {
						return istanbul.RoundChangeCertificate{}
					}
					msg.Signature, err = backend.Sign(data)
					//t.Errorf("signature: signer: %v, address: %v, err: %v", signer, backend.address, err)
					roundChangeCertificate.RoundChangeMessages = append(roundChangeCertificate.RoundChangeMessages, msg)
				}
				return roundChangeCertificate
			}(),
			nil,
		},
		{
			// Empty certificate
			istanbul.RoundChangeCertificate{},
			errInvalidRoundChangeCertificateNumMsgs,
		},
	}
	for _, test := range testCases {
		for _, backend := range sys.backends {
			c := backend.engine.(*core)
			_, err := c.ValidateRoundChangeCertificate(test.certificate)
			if err != test.expectedErr {
				t.Errorf("error mismatch: have %v, want %v", err, test.expectedErr)
			}
		}
	}
}

func TestHandlePreprepare(t *testing.T) {
	N := uint64(4) // replica 0 is the proposer, it will send messages to others
	F := uint64(1) // F does not affect tests

	testCases := []struct {
		system          *testSystem
		expectedRequest istanbul.Proposal
		expectedErr     error
		existingBlock   bool
	}{
		{
			// normal case
			func() *testSystem {
				sys := NewTestSystemWithBackend(N, F)

				for i, backend := range sys.backends {
					c := backend.engine.(*core)
					c.valSet = backend.peers
					if i != 0 {
						c.state = StateAcceptRequest
					}
				}
				return sys
			}(),
			newTestProposal(),
			nil,
			false,
		},
		{
			// future message
			func() *testSystem {
				sys := NewTestSystemWithBackend(N, F)

				for i, backend := range sys.backends {
					c := backend.engine.(*core)
					c.valSet = backend.peers
					if i != 0 {
						c.state = StateAcceptRequest
						// hack: force set subject that future message can be simulated
						c.current = newTestRoundState(
							&istanbul.View{
								Round:    big.NewInt(0),
								Sequence: big.NewInt(0),
							},
							c.valSet,
						)

					} else {
						c.current.SetSequence(big.NewInt(10))
					}
				}
				return sys
			}(),
			makeBlock(1),
			errFutureMessage,
			false,
		},
		{
			// non-proposer
			func() *testSystem {
				sys := NewTestSystemWithBackend(N, F)

				// force remove replica 0, let replica 1 be the proposer
				sys.backends = sys.backends[1:]

				for i, backend := range sys.backends {
					c := backend.engine.(*core)
					c.valSet = backend.peers
					if i != 0 {
						// replica 0 is the proposer
						c.state = StatePreprepared
					}
				}
				return sys
			}(),
			makeBlock(1),
			errNotFromProposer,
			false,
		},
		{
			// errOldMessage
			func() *testSystem {
				sys := NewTestSystemWithBackend(N, F)

				for i, backend := range sys.backends {
					c := backend.engine.(*core)
					c.valSet = backend.peers
					if i != 0 {
						c.state = StatePreprepared
						c.current.SetSequence(big.NewInt(10))
						c.current.SetRound(big.NewInt(10))
					}
				}
				return sys
			}(),
			makeBlock(1),
			errOldMessage,
			false,
		},
	}

OUTER:
	for _, test := range testCases {
		test.system.Run(false)

		v0 := test.system.backends[0]
		r0 := v0.engine.(*core)

		curView := r0.currentView()

		preprepare := &istanbul.Preprepare{
			View:     curView,
			Proposal: test.expectedRequest,
		}

		for i, v := range test.system.backends {
			// i == 0 is primary backend, it is responsible for send PRE-PREPARE messages to others.
			if i == 0 {
				continue
			}

			c := v.engine.(*core)

			m, _ := Encode(preprepare)
			_, val := r0.valSet.GetByAddress(v0.Address())
			// run each backends and verify handlePreprepare function.
			if err := c.handlePreprepare(&istanbul.Message{
				Code:    istanbul.MsgPreprepare,
				Msg:     m,
				Address: v0.Address(),
			}, val); err != nil {
				if err != test.expectedErr {
					t.Errorf("error mismatch: have %v, want %v", err, test.expectedErr)
				}
				continue OUTER
			}

			if c.state != StatePreprepared {
				t.Errorf("state mismatch: have %v, want %v", c.state, StatePreprepared)
			}

			if !test.existingBlock && !reflect.DeepEqual(c.current.Subject().View, curView) {
				t.Errorf("view mismatch: have %v, want %v", c.current.Subject().View, curView)
			}

			// verify prepare messages
			decodedMsg := new(istanbul.Message)
			err := decodedMsg.FromPayload(v.sentMsgs[0], nil)
			if err != nil {
				t.Errorf("error mismatch: have %v, want nil", err)
			}

			expectedCode := istanbul.MsgPrepare
			if test.existingBlock {
				expectedCode = istanbul.MsgCommit
			}
			if decodedMsg.Code != expectedCode {
				t.Errorf("message code mismatch: have %v, want %v", decodedMsg.Code, expectedCode)
			}

			var subject *istanbul.Subject
			err = decodedMsg.Decode(&subject)
			if err != nil {
				t.Errorf("error mismatch: have %v, want nil", err)
			}
			if !test.existingBlock && !reflect.DeepEqual(subject, c.current.Subject()) {
				t.Errorf("subject mismatch: have %v, want %v", subject, c.current.Subject())
			}

		}
	}
}
