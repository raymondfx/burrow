package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hyperledger/burrow/genesis"
	"github.com/hyperledger/burrow/keys"
	"github.com/hyperledger/burrow/permission"
	ptypes "github.com/hyperledger/burrow/permission/types"
)

const DefaultAmount uint64 = 1000000
const DefaultPower uint64 = 10000

// A GenesisSpec is schematic representation of a genesis state, that is it is a template
// for a GenesisDoc excluding that which needs to be instantiated at the point of genesis
// so it describes the type and number of accounts, the genesis salt, but not the
// account keys or addresses, or the GenesisTime. It is responsible for generating keys
// by interacting with the KeysClient it is passed and other information not known at
// specification time
type GenesisSpec struct {
	GenesisTime       *time.Time        `json:",omitempty" toml:",omitempty"`
	ChainName         string            `json:",omitempty" toml:",omitempty"`
	Salt              []byte            `json:",omitempty" toml:",omitempty"`
	GlobalPermissions []string          `json:",omitempty" toml:",omitempty"`
	Accounts          []TemplateAccount `json:",omitempty" toml:",omitempty"`
}

func (gs *GenesisSpec) RealiseKeys(keyClient keys.KeyClient) error {
	for _, templateAccount := range gs.Accounts {
		_, _, err := templateAccount.RealisePubKeyAndAddress(keyClient)
		if err != nil {
			return err
		}
	}
	return nil
}

// Produce a fully realised GenesisDoc from a template GenesisDoc that may omit values
func (gs *GenesisSpec) GenesisDoc(keyClient keys.KeyClient, generateNodeKeys bool) (*genesis.GenesisDoc, error) {
	genesisDoc := new(genesis.GenesisDoc)
	if gs.GenesisTime == nil {
		genesisDoc.GenesisTime = time.Now()
	} else {
		genesisDoc.GenesisTime = *gs.GenesisTime
	}

	if gs.ChainName == "" {
		genesisDoc.ChainName = fmt.Sprintf("BurrowChain_%X", gs.ShortHash())
	} else {
		genesisDoc.ChainName = gs.ChainName
	}

	if len(gs.GlobalPermissions) == 0 {
		genesisDoc.GlobalPermissions = permission.DefaultAccountPermissions.Clone()
	} else {
		basePerms, err := permission.BasePermissionsFromStringList(gs.GlobalPermissions)
		if err != nil {
			return nil, err
		}
		genesisDoc.GlobalPermissions = ptypes.AccountPermissions{
			Base: basePerms,
		}
	}

	templateAccounts := gs.Accounts
	if len(gs.Accounts) == 0 {
		Power := DefaultPower
		templateAccounts = append(templateAccounts, TemplateAccount{
			Power: &Power,
		})
	}

	for i, templateAccount := range templateAccounts {
		account, err := templateAccount.Account(keyClient, i)
		if err != nil {
			return nil, fmt.Errorf("could not create Account from template: %v", err)
		}
		genesisDoc.Accounts = append(genesisDoc.Accounts, *account)
		// Create a corresponding validator
		if templateAccount.Power != nil {
			// Note this does not modify the input template
			templateAccount.Address = &account.Address
			validator, err := templateAccount.Validator(keyClient, i, generateNodeKeys)
			if err != nil {
				return nil, fmt.Errorf("could not create Validator from template: %v", err)
			}
			genesisDoc.Validators = append(genesisDoc.Validators, *validator)
		}
	}

	return genesisDoc, nil
}

func (gs *GenesisSpec) JSONBytes() ([]byte, error) {
	bs, err := json.Marshal(gs)
	if err != nil {
		return nil, err
	}
	// rewrite buffer with indentation
	indentedBuffer := new(bytes.Buffer)
	if err := json.Indent(indentedBuffer, bs, "", "\t"); err != nil {
		return nil, err
	}
	return indentedBuffer.Bytes(), nil
}

func (gs *GenesisSpec) Hash() []byte {
	gsBytes, err := gs.JSONBytes()
	if err != nil {
		panic(fmt.Errorf("could not create hash of GenesisDoc: %v", err))
	}
	hasher := sha256.New()
	hasher.Write(gsBytes)
	return hasher.Sum(nil)
}

func (gs *GenesisSpec) ShortHash() []byte {
	return gs.Hash()[:genesis.ShortHashSuffixBytes]
}

func GenesisSpecFromJSON(jsonBlob []byte) (*GenesisSpec, error) {
	genDoc := new(GenesisSpec)
	err := json.Unmarshal(jsonBlob, genDoc)
	if err != nil {
		return nil, fmt.Errorf("couldn't read GenesisSpec: %v", err)
	}
	return genDoc, nil
}

func accountNameFromIndex(index int) string {
	return fmt.Sprintf("Account_%v", index)
}
