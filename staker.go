package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/eoscanada/eos-go"
	"github.com/eoscanada/eos-go/ecc"
)

var config = &botConfiguration{}
var api *eos.API

type botConfiguration struct {
	Config struct {
		Pkey       string  `toml:"pkey"`
		Account    string  `toml:"account"`
		Mode       string  `toml:"mode"`
		WaxNode    string  `toml:"wax_node"`
		ChunkSize  int     `toml:"chunk_size"`
		UseBalance float64 `toml:"use_balance"`
	} `toml:"config"`
}

func (b *botConfiguration) UseBalanceInt64() int64 {
	return int64(b.Config.UseBalance * 100_000_000.0)
}

func loadCfg() {
	if _, err := toml.DecodeFile("config.toml", config); err != nil {
		fmt.Println(fmt.Errorf("loading config.toml: %w", err))
		os.Exit(1)
	}
}

func remove(s []string, e string) []string {
	for i, a := range s {
		if a == e {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}

func wrapAct(account eos.AccountName, name eos.ActionName, caller eos.AccountName) *eos.Action {
	authorization := make([]eos.PermissionLevel, 0)
	authorization = append(
		authorization, eos.PermissionLevel{
			Actor:      caller,
			Permission: "active",
		},
	)
	return &eos.Action{
		Account:       account,
		Name:          name,
		Authorization: authorization,
	}
}

func FullActionDelegateBW(from eos.AccountName, to eos.AccountName, cpuQuantity eos.Asset, netQuantity eos.Asset) *eos.Action {
	act := wrapAct(eos.AN("eosio"), eos.ActN("delegatebw"), from)
	act.ActionData = eos.NewActionData(
		struct {
			From     eos.AccountName `json:"from"`
			To       eos.AccountName `json:"to"`
			Net      eos.Asset       `json:"stake_net_quantity"`
			Cpu      eos.Asset       `json:"stake_cpu_quantity"`
			Transfer bool            `json:"transfer"`
		}{
			From:     from,
			To:       to,
			Net:      netQuantity,
			Cpu:      cpuQuantity,
			Transfer: false,
		},
	)
	return act
}

func MakeAndSignTransaction(actions []*eos.Action, keys []string) *eos.PackedTransaction {
	var txOpts *eos.TxOptions
	for {
		txOpts = &eos.TxOptions{}
		if err := txOpts.FillFromChain(context.Background(), api); err != nil {
			fmt.Println(fmt.Errorf("filling tx opts: %w", err))
			time.Sleep(time.Millisecond * 5)
			continue
		}
		break
	}
	tx := eos.NewTransaction(actions, txOpts)
	tx.SetExpiration(time.Minute * 55)
	stx := eos.NewSignedTransaction(tx)
	packed, err := stx.Pack(eos.CompressionNone)
	if err != nil {
		panic(err)
	}
	for _, key := range keys {
		pkey, _ := ecc.NewPrivateKey(key)
		txdata, cfd, _ := stx.PackedTransactionAndCFD()
		sigDigest := eos.SigDigest(txOpts.ChainID, txdata, cfd)
		sig, _ := pkey.Sign(sigDigest)
		packed.Signatures = append(packed.Signatures, sig)
	}
	return packed
}

func main() {
	loadCfg()
	// load accounts from accounts.txt
	accounts := make([]string, 0)
	file, err := os.Open("accounts.txt")
	if err != nil {
		fmt.Println(fmt.Errorf("opening accounts.txt: %w", err))
		os.Exit(1)
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if len(scanner.Text()) > 2 {
			accounts = append(accounts, scanner.Text())
		}
	}
	_ = file.Close()
	// read done.txt
	file, err = os.Open("done.txt")
	if err == nil {
		scanner = bufio.NewScanner(file)
		for scanner.Scan() {
			if len(scanner.Text()) > 2 {
				remove(accounts, scanner.Text())
			}
		}
		fmt.Println("Loaded accounts: ", len(accounts))
		_ = file.Close()
	} else {
		file, err = os.Create("done.txt")
		_ = file.Close()
		if err != nil {
			fmt.Println(fmt.Errorf("creating done.txt: %w", err))
			os.Exit(1)
		}
	}

	// split accounts into chunks
	chunks := make([][]string, 0)
	for i := 0; i < len(accounts); i += config.Config.ChunkSize {
		if i+config.Config.ChunkSize > len(accounts) {
			chunks = append(chunks, accounts[i:])
		} else {
			chunks = append(chunks, accounts[i:i+config.Config.ChunkSize])
		}
	}
	fmt.Println("Chunks: ", len(chunks))
	api = eos.New(config.Config.WaxNode)
	acc, err := api.GetCurrencyBalance(context.Background(), eos.AN(config.Config.Account), "WAX", "eosio.token")
	waxBalance := acc[0].Amount
	if int64(waxBalance) < config.UseBalanceInt64() {
		fmt.Println("Not enough WAX balance")
		os.Exit(1)
	}
	fmt.Println("WAX balance: ", float64(waxBalance)/100_000_000.0)
	fmt.Println("Using WAX balance: ", float64(config.UseBalanceInt64())/100_000_000.0)

	if len(chunks) == 0 {
		fmt.Println("No chunks to process.")
		os.Exit(0)
	}
	perAccount := config.UseBalanceInt64() / int64(len(accounts))
	fmt.Printf("CPU per account: %.8f\n", float64(perAccount)/100_000_000.0)
	cpuAsset := eos.Asset{
		Amount: eos.Int64(perAccount),
		Symbol: eos.Symbol{
			Precision: 8,
			Symbol:    "WAX",
		},
	}
	netAsset := eos.Asset{
		Amount: 0,
		Symbol: cpuAsset.Symbol,
	}
	for i, chunk := range chunks {
		firstAcc := chunk[0]
		fmt.Println("Processing chunk #", i)
		wasFirstAcc, err := api.GetAccount(context.Background(), eos.AN(firstAcc))
		if err != nil {
			fmt.Println(fmt.Errorf("getting account %s: %w", firstAcc, err))
			os.Exit(1)
		}
	retry:
		actions := make([]*eos.Action, 0)
		for _, account := range chunk {
			actions = append(actions, FullActionDelegateBW(eos.AN(config.Config.Account), eos.AN(account), cpuAsset, netAsset))
		}
		packed := MakeAndSignTransaction(actions, []string{config.Config.Pkey})
		resp, err := api.PushTransaction(context.Background(), packed)
		if err != nil {
			fmt.Println(fmt.Errorf("pushing transaction: %w", err))
			os.Exit(1)
		}
		fmt.Println("Transaction ID: ", resp.TransactionID, ", waiting 1.5 seconds and validating...")
		time.Sleep(time.Millisecond * 1500)
		firstValidation := true
	retryValidate:
		fmt.Println("Validating...")
		nowFirstAcc, err := api.GetAccount(context.Background(), eos.AN(firstAcc))
		if err != nil {
			fmt.Println(fmt.Errorf("getting account %s: %w", firstAcc, err))
			os.Exit(1)
		}
		if nowFirstAcc.CPUWeight == wasFirstAcc.CPUWeight {
			fmt.Println("Transaction failed (CPU weight not changed)")
			if firstValidation {
				fmt.Println("Retrying validation in 3.5 seconds...")
				time.Sleep(time.Millisecond * 3500)
				firstValidation = false
				goto retryValidate
			}
			fmt.Println("Could not validate transaction. Re-sending it.")
			goto retry
		}
		fmt.Println("Transaction validated.")
		file, err = os.OpenFile("done.txt", os.O_APPEND|os.O_WRONLY, 0600)
		for _, account := range chunk {
			_, _ = file.WriteString(account + "\n")
		}
		_ = file.Close()
	}
}
