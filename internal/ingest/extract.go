package ingest

import (
	"fmt"
	"strings"

	"github.com/albttx/bubblegno.xyz/internal/onbloc"
)

type AccountKind string

const (
	KindEOA     AccountKind = "EOA"
	KindRealm   AccountKind = "REALM"
	KindPackage AccountKind = "PACKAGE"
	KindUnknown AccountKind = "UNKNOWN"
)

type AccountRef struct {
	Address string
	Kind    AccountKind
	PkgPath string
}

type TransferEvent struct {
	UIDKey  string
	Hash    string
	Height  int64
	Index   int64
	Amount  int64
	Denom   string
	From    AccountRef
	To      AccountRef
}

type CallEvent struct {
	UIDKey  string
	Hash    string
	Height  int64
	Index   int64
	Func    string
	PkgPath string
	Send    string
	Caller  AccountRef
	Realm   AccountRef
}

type DeployEvent struct {
	UIDKey  string
	Hash    string
	Height  int64
	PkgPath string
	Creator AccountRef
	Package AccountRef
}

type Batch struct {
	Accounts  map[string]AccountRef // address -> ref
	Transfers []TransferEvent
	Calls     []CallEvent
	Deploys   []DeployEvent
}

func NewBatch() *Batch {
	return &Batch{Accounts: make(map[string]AccountRef)}
}

func (b *Batch) addAccount(a AccountRef) {
	if a.Address == "" {
		return
	}
	if prev, ok := b.Accounts[a.Address]; ok {
		// prefer more specific kind / pkgPath
		if prev.Kind == KindUnknown && a.Kind != KindUnknown {
			b.Accounts[a.Address] = a
			return
		}
		if prev.PkgPath == "" && a.PkgPath != "" {
			b.Accounts[a.Address] = a
		}
		return
	}
	b.Accounts[a.Address] = a
}

func eoa(addr string) AccountRef {
	return AccountRef{Address: addr, Kind: KindEOA}
}

func realm(pkgPath string) AccountRef {
	return AccountRef{
		Address: pkgPath, // graph node id for realms = pkg path (stable)
		Kind:    KindRealm,
		PkgPath: pkgPath,
	}
}

func pkgNode(pkgPath string) AccountRef {
	kind := KindPackage
	if strings.Contains(pkgPath, "/r/") {
		kind = KindRealm
	}
	return AccountRef{Address: pkgPath, Kind: kind, PkgPath: pkgPath}
}

// Extract turns indexer txs into graph events.
func Extract(txs []onbloc.Transaction) *Batch {
	b := NewBatch()
	for _, tx := range txs {
		for i, msg := range tx.Messages {
			switch msg.Value.Typename {
			case "BankMsgSend":
				amt, denom := ParseCoin(msg.Value.Amount)
				from := eoa(msg.Value.FromAddress)
				to := eoa(msg.Value.ToAddress)
				b.addAccount(from)
				b.addAccount(to)
				b.Transfers = append(b.Transfers, TransferEvent{
					UIDKey: fmt.Sprintf("xfer:%s:%d", tx.Hash, i),
					Hash:   tx.Hash,
					Height: tx.BlockHeight,
					Index:  int64(i),
					Amount: amt,
					Denom:  denom,
					From:   from,
					To:     to,
				})
			case "MsgCall":
				caller := eoa(msg.Value.Caller)
				rlm := realm(msg.Value.PkgPath)
				b.addAccount(caller)
				b.addAccount(rlm)
				b.Calls = append(b.Calls, CallEvent{
					UIDKey:  fmt.Sprintf("call:%s:%d", tx.Hash, i),
					Hash:    tx.Hash,
					Height:  tx.BlockHeight,
					Index:   int64(i),
					Func:    msg.Value.Func,
					PkgPath: msg.Value.PkgPath,
					Send:    msg.Value.Send,
					Caller:  caller,
					Realm:   rlm,
				})
				if sendAmt, denom := ParseCoin(msg.Value.Send); sendAmt > 0 {
					b.Transfers = append(b.Transfers, TransferEvent{
						UIDKey: fmt.Sprintf("callsend:%s:%d", tx.Hash, i),
						Hash:   tx.Hash,
						Height: tx.BlockHeight,
						Index:  int64(i),
						Amount: sendAmt,
						Denom:  denom,
						From:   caller,
						To:     rlm,
					})
				}
			case "MsgAddPackage":
				path := ""
				if msg.Value.Package != nil {
					path = msg.Value.Package.Path
				}
				creator := eoa(msg.Value.Creator)
				pkg := pkgNode(path)
				b.addAccount(creator)
				b.addAccount(pkg)
				b.Deploys = append(b.Deploys, DeployEvent{
					UIDKey:  fmt.Sprintf("deploy:%s:%d", tx.Hash, i),
					Hash:    tx.Hash,
					Height:  tx.BlockHeight,
					PkgPath: path,
					Creator: creator,
					Package: pkg,
				})
			}
		}
	}
	return b
}
