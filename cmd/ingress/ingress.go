package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/big"
	netHttp "net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/republicprotocol/republic-go/blockchain/ethereum"
	"github.com/republicprotocol/republic-go/blockchain/ethereum/dnr"
	"github.com/republicprotocol/republic-go/blockchain/ethereum/ledger"
	"github.com/republicprotocol/republic-go/cal"
	"github.com/republicprotocol/republic-go/crypto"
	"github.com/republicprotocol/republic-go/dht"
	"github.com/republicprotocol/republic-go/grpc"
	"github.com/republicprotocol/republic-go/http"
	"github.com/republicprotocol/republic-go/http/adapter"
	"github.com/republicprotocol/republic-go/identity"
	"github.com/republicprotocol/republic-go/ingress"
	"github.com/republicprotocol/republic-go/swarm"
)

type config struct {
	EthereumConfig      ethereum.Config         `json:"ethereum"`
	BootstrapMultiAddrs identity.MultiAddresses `json:"bootstrapMultiAddresses"`
}

func main() {
	done := make(chan struct{})
	defer close(done)

	configEnv := os.Getenv("ENV_CONFIG")
	if configEnv == "" {
		configEnv = "config.json"
	}
	keystoreEnv := os.Getenv("ENV_KEYSTORE")
	if keystoreEnv == "" {
		keystoreEnv = "keystore.json"
	}
	keystorePassphraseEnv := os.Getenv("ENV_KEYSTORE_PASSPHRASE")

	flag.Parse()

	config, err := loadConfig(configEnv)
	if err != nil {
		log.Fatalf("cannot load config: %v", err)
	}

	keystore, err := loadKeystore(keystoreEnv, keystorePassphraseEnv)
	if err != nil {
		log.Fatalf("cannot load keystore: %v", err)
	}

	multiAddr, err := getMultiaddress(keystore, os.Getenv("PORT"))
	if err != nil {
		log.Fatalf("cannot get multi-address: %v", err)
	}

	auth, registry, renLedger, err := getSmartContracts(config.EthereumConfig, keystore)
	if err != nil {
		fmt.Println(fmt.Errorf("cannot get registry: %s", err))
		return
	}

	dht := dht.NewDHT(multiAddr.Address(), 100)
	connPool := grpc.NewConnPool(100)
	swarmClient := grpc.NewSwarmClient(multiAddr, &connPool)
	swarmer := swarm.NewSwarmer(swarmClient, &dht)
	orderbookClient := grpc.NewOrderbookClient(&connPool)
	ingresser := ingress.NewIngress(&registry, renLedger, swarmer, orderbookClient)
	ingressAdapter := adapter.NewIngressAdapter(ingresser)
	openOrderErrors := ingresser.OpenOrderProcess(done)
	openOrderFragmentErrors := ingresser.OpenOrderFragmentsProcess(done)

	go func() {
		for err := range openOrderErrors {
			log.Printf("error processing order: %v", err)
		}
	}()

	go func() {
		for err := range openOrderFragmentErrors {
			log.Printf("error processing order fragment: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := swarmer.Bootstrap(ctx, config.BootstrapMultiAddrs); err != nil {
		log.Printf("error bootstrapping: %v", err)
	}
	ingresser.Sync()

	log.Printf("address %v", multiAddr)
	log.Printf("ethereum %v", auth.From.Hex())
	log.Printf("peers %v", len(dht.MultiAddresses()))
	for _, multiAddr := range dht.MultiAddresses() {
		log.Printf("  %v", multiAddr)
	}
	log.Printf("listening at 0.0.0.0:%v...", os.Getenv("PORT"))
	if err := netHttp.ListenAndServe(fmt.Sprintf("0.0.0.0:%v", os.Getenv("PORT")), http.NewServer(&ingressAdapter, &ingressAdapter)); err != nil {
		log.Fatalf("error listening and serving: %v", err)
	}
}

func getMultiaddress(keystore crypto.Keystore, port string) (identity.MultiAddress, error) {
	// Get our IP address
	ipInfoOut, err := exec.Command("curl", "https://ipinfo.io/ip").Output()
	if err != nil {
		return identity.MultiAddress{}, err
	}
	ipAddress := strings.Trim(string(ipInfoOut), "\n ")
	ingressMultiaddress, err := identity.NewMultiAddressFromString(fmt.Sprintf("/ip4/%s/tcp/%s/republic/%s", ipAddress, port, keystore.Address()))
	if err != nil {
		return identity.MultiAddress{}, fmt.Errorf("cannot obtain trader multi address %v", err)
	}
	return ingressMultiaddress, nil
}

func getSmartContracts(ethereumConfig ethereum.Config, keystore crypto.Keystore) (*bind.TransactOpts, dnr.DarknodeRegistry, cal.RenLedger, error) {
	conn, err := ethereum.Connect(ethereumConfig)
	if err != nil {
		fmt.Println(fmt.Errorf("cannot connect to ethereum: %v", err))
		return nil, dnr.DarknodeRegistry{}, nil, err
	}
	auth := bind.NewKeyedTransactor(keystore.EcdsaKey.PrivateKey)
	auth.GasPrice = big.NewInt(1000000000)

	registry, err := dnr.NewDarknodeRegistry(context.Background(), conn, auth, &bind.CallOpts{})
	if err != nil {
		fmt.Println(fmt.Errorf("cannot bind to darkpool: %v", err))
		return auth, dnr.DarknodeRegistry{}, nil, err
	}

	renLedger, err := ledger.NewRenLedgerContract(context.Background(), conn, auth, &bind.CallOpts{})
	if err != nil {
		fmt.Println(fmt.Errorf("cannot bind to ren ledger: %v", err))
		return auth, dnr.DarknodeRegistry{}, nil, err
	}

	return auth, registry, &renLedger, nil
}

func loadConfig(configFile string) (config, error) {
	file, err := os.Open(configFile)
	if err != nil {
		return config{}, err
	}
	defer file.Close()
	c := config{}
	if err := json.NewDecoder(file).Decode(&c); err != nil {
		return config{}, err
	}
	return c, nil
}

func loadKeystore(keystoreFile, passphrase string) (crypto.Keystore, error) {
	file, err := os.Open(keystoreFile)
	if err != nil {
		return crypto.Keystore{}, err
	}
	defer file.Close()

	if passphrase == "" {
		keystore := crypto.Keystore{}
		if err := json.NewDecoder(file).Decode(&keystore); err != nil {
			return keystore, err
		}
		return keystore, nil
	}

	keystore := crypto.Keystore{}
	keystoreData, err := ioutil.ReadAll(file)
	if err != nil {
		return keystore, err
	}
	if err := keystore.DecryptFromJSON(keystoreData, passphrase); err != nil {
		return keystore, err
	}
	return keystore, nil
}
