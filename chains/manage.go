package chains

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/eris-ltd/eris-cli/data"
	"github.com/eris-ltd/eris-cli/definitions"
	"github.com/eris-ltd/eris-cli/loaders"
	"github.com/eris-ltd/eris-cli/perform"
	"github.com/eris-ltd/eris-cli/services"
	"github.com/eris-ltd/eris-cli/util"
	"github.com/eris-ltd/eris-cli/version"

	log "github.com/eris-ltd/eris-cli/Godeps/_workspace/src/github.com/Sirupsen/logrus"
	. "github.com/eris-ltd/eris-cli/Godeps/_workspace/src/github.com/eris-ltd/common/go/common"
	"github.com/eris-ltd/eris-cli/Godeps/_workspace/src/github.com/eris-ltd/common/go/ipfs"
)

func MakeChain(do *definitions.Do) error {
	var eie bool = false
	if os.Getenv("ERIS_IN_DOCKER") == "1" {
		log.Info("Using Eris in Docker")
		eie = true
	}
	do.Service.Name = do.Name
	do.Service.Image = version.ERIS_IMG_CM
	do.Service.User = "eris"
	do.Service.Links = []string{fmt.Sprintf("%s:%s", util.ServiceContainersName("keys", 1), "keys")}
	if eie {
		do.Service.AutoData = true
	} else {
		do.Service.AutoData = false
		do.Service.Volumes = []string{fmt.Sprintf("%s:%s", ChainsPath, path.Join(ErisContainerRoot, "chains"))} // using path.Join here because even when host on windows we want / not \
	}

	if do.Known {
		log.Debug("Using MintGen rather than eris:cm")
		do.Service.EntryPoint = "mintgen"
		do.Service.Command = fmt.Sprintf("known %s --csv=%s,%s > %s", do.Name, do.ChainMakeVals, do.ChainMakeActs, path.Join(ErisContainerRoot, "chains", do.Name, "genesis.json"))
	} else {
		log.Debug("Using eris:cm rather than MintGen")
		do.Service.EntryPoint = fmt.Sprintf("eris-cm make %s", do.Name)
	}

	do.Service.Environment = []string{
		fmt.Sprintf("ERIS_KEYS_PATH=http://keys:%d", 4767), // note, needs to be made aware of keys port...
		fmt.Sprintf("ERIS_CHAINMANAGER_ACCOUNTTYPES=%s", strings.Join(do.AccountTypes, ",")),
		fmt.Sprintf("ERIS_CHAINMANAGER_CHAINTYPE=%s", do.ChainType),
		fmt.Sprintf("ERIS_CHAINMANAGER_TARBALLS=%v", do.Tarball),
		fmt.Sprintf("ERIS_CHAINMANAGER_ZIPFILES=%v", do.ZipFile),
		fmt.Sprintf("ERIS_CHAINMANAGER_OUTPUT=%v", do.Output),
		fmt.Sprintf("ERIS_CHAINMANAGER_VERBOSE=%v", do.Verbose),
		fmt.Sprintf("ERIS_CHAINMANAGER_DEBUG=%v", do.Debug),
	}

	if !do.Known && len(do.AccountTypes) == 0 && do.ChainType == "" {
		do.Operations.Interactive = true
		do.Operations.Args = strings.Split(do.Service.EntryPoint, " ")
	}

	if do.Known {
		do.Operations.Args = append(do.Operations.Args, strings.Split(do.Service.Command, " ")...)
		do.Service.WorkDir = path.Join(ErisContainerRoot, "chains", do.Name)
	}

	do.Operations.ContainerType = "service"
	do.Operations.SrvContainerName = util.ServiceContainersName(do.Name, 1)
	do.Operations.DataContainerName = util.DataContainersName(do.Name, 1)
	do.Operations.ContainerNumber = 1
	do.Operations.Remove = true

	// import data; if data
	doData := definitions.NowDo()
	doData.Name = do.Name
	doData.Operations.ContainerNumber = do.Operations.ContainerNumber
	defer data.RmData(doData)

	if eie {
		// first. the fake one.
		doData.Source = filepath.Join(ChainsPath, "HEAD")
		doData.Destination = ErisContainerRoot
		if err := data.ImportData(doData); err != nil {
			return err
		}
		doData.Operations.Args = []string{"mkdir", "-p", path.Join(ErisContainerRoot, "chains")}
		if err := data.ExecData(doData); err != nil {
			return err
		}
		doData.Operations.Args = []string{}
		// second. the real one.
		doData.Source = ChainsPath
		doData.Destination = path.Join(ErisContainerRoot, "chains")
		if err := data.ImportData(doData); err != nil {
			return err
		}
	}

	if err := perform.DockerExecService(do.Service, do.Operations); err != nil {
		return err
	}

	if eie {
		// export data
		doData.Destination = ErisRoot
		doData.Source = path.Join(ErisContainerRoot, "chains")
		if err := data.ExportData(doData); err != nil {
			return err
		}
	}

	return nil
}

func RegisterChain(do *definitions.Do) error {
	// do.Name is mandatory
	if do.Name == "" {
		return fmt.Errorf("RegisterChain requires a chainame")
	}
	etcbChain := do.ChainID
	do.ChainID = do.Name

	// NOTE: registration expects you to have the data container
	if !util.IsDataContainer(do.Name, do.Operations.ContainerNumber) {
		return fmt.Errorf("Registration requires you to have a data container for the chain. Could not find data for %s", do.Name)
	}

	chain, err := loaders.LoadChainDefinition(do.Name, false, do.Operations.ContainerNumber)
	if err != nil {
		return err
	}
	log.WithField("image", chain.Service.Image).Debug("Chain loaded")

	// set chainid and other vars
	envVars := []string{
		fmt.Sprintf("CHAIN_ID=%s", do.ChainID),                 // of the etcb chain
		fmt.Sprintf("PUBKEY=%s", do.Pubkey),                    // pubkey to register chain with
		fmt.Sprintf("ETCB_CHAIN_ID=%s", etcbChain),             // chain id of the etcb chain
		fmt.Sprintf("NODE_ADDR=%s", do.Gateway),                // etcb node to send the register tx to
		fmt.Sprintf("NEW_P2P_SEEDS=%s", do.Operations.Args[0]), // seeds to register for the chain // TODO: deal with multi seed (needs support in tendermint)
	}
	envVars = append(envVars, do.Env...)

	log.WithFields(log.Fields{
		"environment": envVars,
		"links":       do.Links,
	}).Debug("Registering chain with")
	chain.Service.Environment = append(chain.Service.Environment, envVars...)
	chain.Service.Links = append(chain.Service.Links, do.Links...)

	if err := bootDependencies(chain, do); err != nil {
		return err
	}

	log.WithFields(log.Fields{
		"=>":    chain.Service.Name,
		"image": chain.Service.Image,
	}).Debug("Performing chain container start")
	chain.Operations = loaders.LoadDataDefinition(chain.Service.Name, do.Operations.ContainerNumber)
	chain.Operations.Args = []string{loaders.ErisChainRegister}

	_, err = perform.DockerRunData(chain.Operations, chain.Service)

	return err
}

func ImportChain(do *definitions.Do) error {
	fileName := filepath.Join(ChainsPath, do.Name)
	if filepath.Ext(fileName) == "" {
		fileName = fileName + ".toml"
	}

	s := strings.Split(do.Path, ":")
	if s[0] == "ipfs" {
		var err error
		if log.GetLevel() > 0 {
			err = ipfs.GetFromIPFS(s[1], fileName, "", os.Stdout)
		} else {
			err = ipfs.GetFromIPFS(s[1], fileName, "", bytes.NewBuffer([]byte{}))
		}

		if err != nil {
			return err
		}
		return nil
	}

	if strings.Contains(s[0], "github") {
		log.Warn("https://twitter.com/ryaneshea/status/595957712040628224")
		return nil
	}

	return fmt.Errorf("I do not know how to get that file. Sorry.")
}

func InspectChain(do *definitions.Do) error {
	chain, err := loaders.LoadChainDefinition(do.Name, false, do.Operations.ContainerNumber)
	if err != nil {
		return err
	}

	if IsChainExisting(chain) {
		log.WithField("=>", chain.Service.Name).Debug("Inspecting chain")
		err := services.InspectServiceByService(chain.Service, chain.Operations, do.Operations.Args[0])
		if err != nil {
			return err
		}
	}

	return nil
}

func LogsChain(do *definitions.Do) error {
	chain, err := loaders.LoadChainDefinition(do.Name, false, do.Operations.ContainerNumber)
	if err != nil {
		return err
	}

	err = perform.DockerLogs(chain.Service, chain.Operations, do.Follow, do.Tail)
	if err != nil {
		return err
	}

	return nil
}

// export a chain definition file
func ExportChain(do *definitions.Do) error {
	chain, err := loaders.LoadChainDefinition(do.Name, false, do.Operations.ContainerNumber)
	if err != nil {
		return err
	}
	if IsChainExisting(chain) {
		doNow := definitions.NowDo()
		doNow.Name = "ipfs"
		services.EnsureRunning(doNow)

		hash, err := exportFile(do.Name)
		if err != nil {
			return err
		}
		log.Warn(hash)

	} else {
		return fmt.Errorf(`I don't known of that chain.
Please retry with a known chain.
To find known chains use: eris chains ls --known`)
	}
	return nil
}

func CheckoutChain(do *definitions.Do) error {
	if do.Name == "" {
		do.Result = "nil"
		return util.NullHead()
	}

	curHead, _ := util.GetHead()
	if do.Name == curHead {
		do.Result = "no change"
		return nil
	}

	return util.ChangeHead(do.Name)
}

func CurrentChain(do *definitions.Do) error {
	head, _ := util.GetHead()

	if head == "" {
		head = "There is no chain checked out."
	}

	log.Warn(head)
	do.Result = head

	return nil
}

func PlopChain(do *definitions.Do) error {
	do.Name = do.ChainID
	rootDir := path.Join(ErisContainerRoot, "chains", do.ChainID)
	switch do.Type {
	case "genesis":
		do.Operations.Args = []string{"cat", path.Join(rootDir, "genesis.json")}
	case "config":
		do.Operations.Args = []string{"cat", path.Join(rootDir, "config.toml")}
	case "status":
		do.Operations.Args = []string{"mintinfo", "--node-addr", "http://0.0.0.0:46657", "status"}
	case "validators":
		do.Operations.Args = []string{"mintinfo", "--node-addr", "http://0.0.0.0:46657", "validators"}
	default:
		return fmt.Errorf("unknown plop option %s", do.Type)
	}
	do.Operations.PublishAllPorts = true // avoid port conflict
	log.WithField("args", do.Operations.Args).Debug("Executing command")
	return ExecChain(do)
}

func PortsChain(do *definitions.Do) error {
	chain, err := loaders.LoadChainDefinition(do.Name, false, do.Operations.ContainerNumber)
	if err != nil {
		return err
	}

	if IsChainExisting(chain) {
		log.WithField("=>", chain.Name).Debug("Getting chain port mapping")
		return util.PrintPortMappings(chain.Operations.SrvContainerID, do.Operations.Args)
	}

	return nil
}

func EditChain(do *definitions.Do) error {
	chainDefFile := util.GetFileByNameAndType("chains", do.Name)
	log.WithField("file", chainDefFile).Info("Editing chain definition")
	do.Result = "success"
	return Editor(chainDefFile)
}

// XXX: What's going on here? => [csk]: magic
func RenameChain(do *definitions.Do) error {
	if do.Name == do.NewName {
		return fmt.Errorf("Cannot rename to same name")
	}

	newNameBase := strings.Replace(do.NewName, filepath.Ext(do.NewName), "", 1)
	transformOnly := newNameBase == do.Name

	if util.IsKnownChain(do.Name) {
		log.WithFields(log.Fields{
			"from": do.Name,
			"to":   do.NewName,
		}).Info("Renaming chain")

		log.WithField("=>", do.Name).Debug("Loading chain definition file")
		chainDef, err := loaders.LoadChainDefinition(do.Name, false, 1) // TODO:CNUM
		if err != nil {
			return err
		}

		if !transformOnly {
			log.Debug("Renaming chain container")
			err = perform.DockerRename(chainDef.Operations, do.NewName)
			if err != nil {
				return err
			}
		}

		oldFile := util.GetFileByNameAndType("chains", do.Name)
		if err != nil {
			return err
		}

		if filepath.Base(oldFile) == do.NewName {
			log.Info("Those are the same file. Not renaming")
			return nil
		}

		log.Debug("Renaming chain definition file")
		var newFile string
		if filepath.Ext(do.NewName) == "" {
			newFile = strings.Replace(oldFile, do.Name, do.NewName, 1)
		} else {
			newFile = filepath.Join(ChainsPath, do.NewName)
		}

		chainDef.Name = newNameBase
		// Generally we won't want to use Service.Name
		// as it will be confused with the Name.
		chainDef.Service.Name = ""
		// Service.Image should be taken from the default.toml.
		chainDef.Service.Image = ""
		err = WriteChainDefinitionFile(chainDef, newFile)
		if err != nil {
			return err
		}

		if !transformOnly {
			log.WithFields(log.Fields{
				"from": fmt.Sprintf("%s:%d", do.Name, chainDef.Operations.ContainerNumber),
				"to":   fmt.Sprintf("%s:%d", do.NewName, chainDef.Operations.ContainerNumber),
			}).Info("Renaming chain data container")
			do.Operations.ContainerNumber = chainDef.Operations.ContainerNumber
			err = data.RenameData(do)
			if err != nil {
				return err
			}
		}

		os.Remove(oldFile)
	} else {
		return fmt.Errorf("I cannot find that chain. Please check the chain name you sent me.")
	}
	return nil
}

func UpdateChain(do *definitions.Do) error {
	chain, err := loaders.LoadChainDefinition(do.Name, false, do.Operations.ContainerNumber)
	if err != nil {
		return err
	}

	// set the right env vars and command
	if IsChainRunning(chain) {
		chain.Service.Environment = []string{fmt.Sprintf("CHAIN_ID=%s", do.Name)}
		chain.Service.Environment = append(chain.Service.Environment, do.Env...)
		chain.Service.Links = append(chain.Service.Links, do.Links...)
		chain.Service.Command = loaders.ErisChainStart
	}

	err = perform.DockerRebuild(chain.Service, chain.Operations, do.Pull, do.Timeout)
	if err != nil {
		return err
	}
	return nil
}

func RmChain(do *definitions.Do) error {
	chain, err := loaders.LoadChainDefinition(do.Name, false, do.Operations.ContainerNumber)
	if err != nil {
		return err
	}

	if IsChainExisting(chain) {
		if err = perform.DockerRemove(chain.Service, chain.Operations, do.RmD, do.Volumes); err != nil {
			return err
		}
	} else {
		log.Info("Chain container does not exist")
	}

	if do.File {
		oldFile := util.GetFileByNameAndType("chains", do.Name)
		if err != nil {
			return err
		}
		log.WithField("file", oldFile).Warn("Removing file")
		if err := os.Remove(oldFile); err != nil {
			return err
		}
	}
	return nil
}

func GraduateChain(do *definitions.Do) error {
	chain, err := loaders.LoadChainDefinition(do.Name, false, 1)
	if err != nil {
		return err
	}

	serv := loaders.ServiceDefFromChain(chain, loaders.ErisChainStart)
	if err := services.WriteServiceDefinitionFile(serv, filepath.Join(ServicesPath, chain.ChainID+".toml")); err != nil {
		return err
	}
	return nil
}

func CatChain(do *definitions.Do) error {
	cat, err := ioutil.ReadFile(filepath.Join(ChainsPath, do.Name+".toml"))
	if err != nil {
		return err
	}
	// Let's actually WRITE this to the GlobalConfig.Writer...
	log.Warn(string(cat))
	return nil

}

func exportFile(chainName string) (string, error) {
	fileName := util.GetFileByNameAndType("chains", chainName)

	var hash string
	var err error
	if log.GetLevel() > 0 {
		hash, err = ipfs.SendToIPFS(fileName, "", os.Stdout)
	} else {
		hash, err = ipfs.SendToIPFS(fileName, "", bytes.NewBuffer([]byte{}))
	}

	if err != nil {
		return "", err
	}

	return hash, nil
}
