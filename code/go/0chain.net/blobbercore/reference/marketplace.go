package reference

import (
	"0chain.net/blobbercore/datastore"
	"0chain.net/core/config"
	. "0chain.net/core/logging"
	"context"
	"github.com/0chain/gosdk/core/zcncrypto"
	"go.uber.org/zap"
)

type MarketplaceInfo struct {
	Mnemonic   string    `gorm:"mnemonic" json:"mnemonic,omitempty"`
	PublicKey   string    `gorm:"public_key" json:"public_key"`
	PrivateKey  string    `gorm:"private_key" json:"private_key,omitempty"`
}

type KeyPairInfo struct {
	PublicKey string
	PrivateKey string
	Mnemonic string
}

func TableName() string {
	return "marketplace"
}

func AddEncryptionKeyPairInfo(ctx context.Context, keyPairInfo KeyPairInfo) error {
	db := datastore.GetStore().GetTransaction(ctx)
	return db.Table(TableName()).Create(&MarketplaceInfo{
		PrivateKey: keyPairInfo.PrivateKey,
		PublicKey: keyPairInfo.PublicKey,
		Mnemonic: keyPairInfo.Mnemonic,
	}).Error
}

func GetMarketplaceInfo(ctx context.Context) (MarketplaceInfo, error) {
	db := datastore.GetStore().GetTransaction(ctx)
	marketplaceInfo := MarketplaceInfo{}
	err := db.Table(TableName()).First(&marketplaceInfo).Error
	return marketplaceInfo, err
}

func GetSecretKeyPair() (*KeyPairInfo, error) {
	//sigScheme := zcncrypto.NewSignatureScheme(config.Configuration.SignatureScheme)
	// TODO: bls0chain scheme crashes
	sigScheme := zcncrypto.NewSignatureScheme("ed25519")
	wallet, err := sigScheme.GenerateKeys()
	if err != nil {
		return nil, err
	}
	return &KeyPairInfo {
		PublicKey: wallet.Keys[0].PublicKey,
		PrivateKey: wallet.Keys[0].PrivateKey,
		Mnemonic: wallet.Mnemonic,
	}, nil
}

func GetOrCreateMarketplaceInfo(ctx context.Context) (*MarketplaceInfo, error) {
	row, err := GetMarketplaceInfo(ctx)
	if err == nil {
		return &row, err
	}

	Logger.Info("Creating key pair", zap.String("signature_scheme", config.Configuration.SignatureScheme))
	keyPairInfo, err := GetSecretKeyPair()
	Logger.Info("Secret key pair created")

	if err != nil {
		return nil, err
	}

	AddEncryptionKeyPairInfo(ctx, *keyPairInfo)

	return &MarketplaceInfo{
		PrivateKey: keyPairInfo.PrivateKey,
		PublicKey: keyPairInfo.PublicKey,
		Mnemonic: keyPairInfo.Mnemonic,
	}, nil
}
