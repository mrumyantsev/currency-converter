package parserd

import (
	"errors"
	"fmt"
	"time"

	"github.com/mrumyantsev/currency-converter/internal/pkg/config"
	dbstorage "github.com/mrumyantsev/currency-converter/internal/pkg/db-storage"
	fsops "github.com/mrumyantsev/currency-converter/internal/pkg/fs-ops"
	httpclient "github.com/mrumyantsev/currency-converter/internal/pkg/http-client"
	httpserver "github.com/mrumyantsev/currency-converter/internal/pkg/http-server"
	memstorage "github.com/mrumyantsev/currency-converter/internal/pkg/mem-storage"
	"github.com/mrumyantsev/currency-converter/internal/pkg/models"
	timechecks "github.com/mrumyantsev/currency-converter/internal/pkg/time-checks"
	"github.com/mrumyantsev/currency-converter/internal/pkg/utils"
	xmlparser "github.com/mrumyantsev/currency-converter/internal/pkg/xml-parser"

	"github.com/mrumyantsev/fastlog"
)

type ParserD struct {
	config     *config.Config
	fsOps      *fsops.FsOps
	httpClient *httpclient.HttpClient
	xmlParser  *xmlparser.XmlParser
	timeChecks *timechecks.TimeChecks
	memStorage *memstorage.MemStorage
	dbStorage  *dbstorage.DbStorage
	httpServer *httpserver.HttpServer
}

func New() *ParserD {
	cfg := config.New()

	err := cfg.Init()
	if err != nil {
		fastlog.Error("cannot initialize configuration", err)
	}

	fastlog.IsEnableDebugLogs = cfg.IsEnableDebugLogs

	memStorage := memstorage.New()

	return &ParserD{
		config:     cfg,
		fsOps:      fsops.New(cfg),
		httpClient: httpclient.New(cfg),
		xmlParser:  xmlparser.New(cfg),
		timeChecks: timechecks.New(cfg),
		memStorage: memStorage,
		dbStorage:  dbstorage.New(cfg),
		httpServer: httpserver.New(cfg, memStorage),
	}
}

func (p *ParserD) SaveCurrencyDataToFile() {
	data, err := p.httpClient.GetCurrencyData()
	if err != nil {
		fastlog.Error("cannot get currencies from web", err)
	}

	err = p.fsOps.OverwriteCurrencyDataFile(data)
	if err != nil {
		fastlog.Error("cannot write currencies to file", err)
	}

	fastlog.Info("currency data saved in file: " + p.config.CurrencySourceFile)
}

func (p *ParserD) Run() {
	var (
		timeToNextUpdate *time.Duration
		err              error
	)

	for {
		p.updateCurrencyDataInStorages()

		timeToNextUpdate, err = p.timeChecks.GetTimeToNextUpdate()
		if err != nil {
			fastlog.Error("cannot get time to next update", err)
		}

		fastlog.Info("next update will occur after " +
			(*timeToNextUpdate).Round(time.Second).String())

		if !p.httpServer.GetIsRunning() {
			go func() {
				err = p.httpServer.Run()
				if err != nil {
					fastlog.Error("cannot run http server", err)
				}
			}()
		}

		time.Sleep(*timeToNextUpdate)
	}
}

func (p *ParserD) updateCurrencyDataInStorages() {
	var (
		latestUpdateDatetime  *models.UpdateDatetime
		latestCurrencyStorage *models.CurrencyStorage
		isNeedUpdate          bool
		currentDatetime       string = time.Now().Format(time.RFC3339)
		err                   error
	)

	err = p.dbStorage.Connect()
	if err != nil {
		fastlog.Error("cannot connect to db to do data update", err)
	}
	defer func() {
		err = p.dbStorage.Disconnect()
		if err != nil {
			fastlog.Error("cannot disconnect from db to do data update", err)
		}
	}()

	fastlog.Info("checking latest update time...")

	latestUpdateDatetime, err = p.dbStorage.GetLatestUpdateDatetime()
	if err != nil {
		fastlog.Error("cannot get current update datetime", err)
	}

	isNeedUpdate, err = p.timeChecks.IsNeedForUpdateDb(latestUpdateDatetime)
	if err != nil {
		fastlog.Error("cannot check is need update for db or not", err)
	}

	if isNeedUpdate {
		fastlog.Info("data is outdated")
		fastlog.Info("initializing update process...")

		latestCurrencyStorage, err = p.getParsedDataFromSource()
		if err != nil {
			fastlog.Error("cannot get parsed data from source", err)
		}

		fastlog.Info("saving data...")

		latestUpdateDatetime, err = p.dbStorage.InsertUpdateDatetime(currentDatetime)
		if err != nil {
			fastlog.Error("cannot insert datetime into db", err)
		}

		err = p.dbStorage.InsertCurrencies(latestCurrencyStorage, latestUpdateDatetime.Id)
		if err != nil {
			fastlog.Error("cannot insert currencies into db", err)
		}
	} else {
		latestCurrencyStorage, err = p.dbStorage.GetLatestCurrencies(latestUpdateDatetime.Id)
		if err != nil {
			fastlog.Error("cannot get currencies from db", err)
		}
	}

	p.memStorage.SetUpdateDatetime(latestUpdateDatetime)
	p.memStorage.SetCurrencyStorage(latestCurrencyStorage)

	fastlog.Info("data is now up to date")
}

func (p *ParserD) getParsedDataFromSource() (*models.CurrencyStorage, error) {
	var (
		currencyData []byte
		err          error
	)

	fastlog.Info("getting new data...")

	if p.config.IsReadCurrencyDataFromFile {
		fastlog.Debug("getting data from local file...")

		currencyData, err = p.fsOps.GetCurrencyData()
		if err != nil {
			return nil, utils.DecorateError("cannot get currencies from file", err)
		}
	} else {
		fastlog.Debug("getting data from web...")

		currencyData, err = p.httpClient.GetCurrencyData()
		if err != nil {
			return nil, utils.DecorateError("cannot get curencies from web", err)
		}
	}

	err = replaceCommasWithDots(currencyData)
	if err != nil {
		return nil, utils.DecorateError("cannot replace commas in data", err)
	}

	fastlog.Info("parsing data...")

	currencyStorage, err := p.xmlParser.Parse(currencyData)
	if err != nil {
		return nil, utils.DecorateError("cannot parse data", err)
	}

	return currencyStorage, nil
}

func replaceCommasWithDots(data []byte) error {
	const (
		START_DATA_INDEX int  = 100
		CHAR_COMMA       byte = ','
		CHAR_DOT         byte = '.'
	)

	if data == nil {
		return errors.New("data is empty")
	}

	lengthOfData := len(data)

	for i := START_DATA_INDEX; i < lengthOfData; i++ {
		if data[i] == CHAR_COMMA {
			data[i] = CHAR_DOT
		}
	}

	return nil
}

// Prints data. For debugging purposes.
func printData(currencyStorage *models.CurrencyStorage) {
	for _, currency := range currencyStorage.Currencies {
		fmt.Println(currency.NumCode)
		fmt.Println(" ", currency.CharCode)
		fmt.Println(" ", currency.Multiplier)
		fmt.Println(" ", currency.Name)
		fmt.Println(" ", currency.CurrencyValue)
	}
}
