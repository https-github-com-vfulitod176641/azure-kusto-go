package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-kusto-go/kusto"
	"github.com/Azure/azure-kusto-go/kusto/data/errors"
	"github.com/Azure/azure-kusto-go/kusto/data/table"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/conn"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/filesystem"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/properties"
	"github.com/Azure/azure-kusto-go/kusto/ingest/internal/resources"
)

var (
	manager   atomic.Value // *resources.Manager
	managerMu sync.Mutex
)

// getManager retrieves a Manager or creates a new one with client. Clients, other than having timeout options,
// are all the same. Managers all have the same context. This acts as a singleton to prevent propogating
// mulitple managers around that have background goroutines running.
func getManager(client *kusto.Client) (*resources.Manager, error) {
	i := manager.Load()
	if i == nil {
		managerMu.Lock()
		defer managerMu.Unlock()

		mgr, err := resources.New(client)
		if err != nil {
			return nil, err
		}
		manager.Store(mgr)
		return mgr, nil
	}
	return i.(*resources.Manager), nil
}

type streamer interface {
	stream(ctx context.Context, format DataFormat, mappingName string) (io.WriteCloser, error)
}

// Ingestion provides data ingestion from external sources into Kusto.
type Ingestion struct {
	db    string
	table string

	client *kusto.Client
	mgr    *resources.Manager

	fs *filesystem.Ingestion

	connMu     sync.Mutex
	streamConn *conn.Conn

	// mappingMu protects mappingNames.
	mappingsMu sync.Mutex
	// mappings stores mappings on the server.
	mappings          map[string]mapEntry
	lastMappingLookup time.Time
}

// New is the constructor for Ingestion.
func New(client *kusto.Client, db, table string) (*Ingestion, error) {
	mgr, err := getManager(client)
	if err != nil {
		return nil, err
	}

	fs, err := filesystem.New(db, table, mgr)
	if err != nil {
		return nil, err
	}

	i := &Ingestion{
		client:   client,
		mgr:      mgr,
		db:       db,
		table:    table,
		fs:       fs,
		mappings: map[string]mapEntry{},
	}

	return i, nil
}

// FileOption is an optional argument to FromFile().
type FileOption interface {
	// TODO(jdoak, daniel): We need to refactor this into options that can work for FileOption and work
	// for ReaderOption(which doesn't exist yet).  Right now we are doing some checks in FromReader() to
	// make sure that the user doesn't pass options we don't like.  But it would be better to have the compiler do this.
	isFileOption()
}

type propertyOption func(p *properties.All) error

func (p propertyOption) isFileOption() {}

// FlushImmediately tells Kusto to flush on write.
func FlushImmediately() FileOption {
	return propertyOption(
		func(p *properties.All) error {
			p.Ingestion.FlushImmediately = true
			return nil
		},
	)
}

// DataFormat indicates what type of encoding format was used for source data.
// Not all options can be used in every method.
type DataFormat = properties.DataFormat

// note: any change here needs to be kept up to date with the properties version.
// I'm not a fan of having two copies, but I don't think it is worth moving to its own package
// to allow properties and ingest to both import without a cycle.
const (
	// DFUnknown indicates the EncodingType is not set.
	DFUnknown DataFormat = properties.DFUnknown
	// AVRO indicates the source is encoded in Apache Avro format.
	AVRO DataFormat = properties.AVRO
	// ApacheAVRO indicates the source is encoded in Apache avro2json format.
	ApacheAVRO DataFormat = properties.ApacheAVRO
	// CSV indicates the source is encoded in comma seperated values.
	CSV DataFormat = properties.CSV
	// JSON indicates the source is encoded as one or more lines, each containing a record in Javscript Object Notation.
	JSON DataFormat = properties.JSON
	// MultiJSON indicates the source is encoded in JSON-Array of individual records in Javscript Object Notation.
	MultiJSON DataFormat = properties.MultiJSON
	// ORC indicates the source is encoded in Apache Optimized Row Columnar format.
	ORC DataFormat = properties.ORC
	// Parquet indicates the source is encoded in Apache Parquet format.
	Parquet DataFormat = properties.Parquet
	// PSV is pipe "|" separated values.
	PSV DataFormat = properties.PSV
	// Raw is a text file that has only a single string value.
	Raw DataFormat = properties.Raw
	// SCSV is a file containing semicolon ";" separated values.
	SCSV DataFormat = properties.SCSV
	// SOHSV is a file containing SOH-separated values(ASCII codepont 1).
	SOHSV DataFormat = properties.SOHSV
	// SStream indicats the source is encoded as a Microsoft Cosmos Structured Streams format
	SStream DataFormat = properties.SStream
	// TSV is a file containing tab seperated values ("\t").
	TSV DataFormat = properties.TSV
	// TSVE is a file containing escaped-tab seperated values ("\t").
	TSVE DataFormat = properties.TSVE
	// TXT is a text file with lines deliminated by "\n".
	TXT DataFormat = properties.TXT
	// WCLogFile indicates the source is encoded using W3C Extended Log File format
	W3CLogFile DataFormat = properties.W3CLogFile
)

// IngestionMapping provides runtime mapping of the data being imported to the fields in the table.
// "ref" will be JSON encoded, so it can be any type that can be JSON marshalled. If you pass a string
// or []byte, it will be interpreted as already being JSON encoded.
// mappingKind can only be: CSV, JSON, AVRO, Parquet or ORC.
func IngestionMapping(mapping interface{}, mappingKind DataFormat) FileOption {
	return propertyOption(
		func(p *properties.All) error {
			if !mappingKind.IsValidMappingKind() {
				return errors.ES(
					errors.OpUnknown,
					errors.KClientArgs,
					"IngestionMapping() option does not support EncodingType %v", mappingKind,
				).SetNoRetry()
			}

			var j string
			switch v := mapping.(type) {
			case string:
				j = v
			case []byte:
				j = string(v)
			default:
				b, err := json.Marshal(mapping)
				if err != nil {
					return errors.ES(
						errors.OpUnknown,
						errors.KClientArgs,
						"IngestMapping option was passed to an Ingest.Ingestion call that was not a string, []byte or could be JSON encoded: %s", err,
					).SetNoRetry()
				}
				j = string(b)
			}

			p.Ingestion.Additional.IngestionMapping = j
			p.Ingestion.Additional.IngestionMappingType = mappingKind

			return nil
		},
	)
}

// IngestionMappingRef provides the name of a pre-created mapping for the data being imported to the fields in the table.
// mappingKind can only be: CSV, JSON, AVRO, Parquet or ORC.
// For more details, see: https://docs.microsoft.com/en-us/azure/kusto/management/create-ingestion-mapping-command
func IngestionMappingRef(refName string, mappingKind DataFormat) FileOption {
	return propertyOption(
		func(p *properties.All) error {
			if !mappingKind.IsValidMappingKind() {
				return errors.ES(errors.OpUnknown, errors.KClientArgs, "IngestionMappingRef() option does not support EncodingType %v", mappingKind).SetNoRetry()
			}
			p.Ingestion.Additional.IngestionMappingRef = refName
			p.Ingestion.Additional.IngestionMappingType = mappingKind
			return nil
		},
	)
}

// DeleteSource deletes the source file from when it has been uploaded to Kusto.
func DeleteSource() FileOption {
	return propertyOption(
		func(p *properties.All) error {
			// You might be asking, what if we are just using blobstore? Well, then this option doesn't matter :)
			p.Source.DeleteLocalSource = true
			return nil
		},
	)
}

// IgnoreSizeLimit ignores the size limit for data ingestion.
func IgnoreSizeLimit() FileOption {
	return propertyOption(
		func(p *properties.All) error {
			// You might be asking, what if we are just using blobstore? Well, then this option doesn't matter :)
			p.Ingestion.IgnoreSizeLimit = true
			return nil
		},
	)
}

// Tags are tags to be associated with the ingested ata.
func Tags(tags []string) FileOption {
	return propertyOption(
		func(p *properties.All) error {
			// You might be asking, what if we are just using blobstore? Well, then this option doesn't matter :)
			p.Ingestion.Additional.Tags = tags
			return nil
		},
	)
}

// IfNotExists provides a string value that, if specified, prevents ingestion from succeeding if the table already
// has data tagged with an ingest-by: tag with the same value. This ensures idempotent data ingestion.
// For more information see: https://docs.microsoft.com/en-us/azure/kusto/management/extents-overview#ingest-by-extent-tags
func IfNotExists(ingestByTag string) FileOption {
	return propertyOption(
		func(p *properties.All) error {
			// You might be asking, what if we are just using blobstore? Well, then this option doesn't matter :)
			p.Ingestion.Additional.IngestIfNotExists = ingestByTag
			return nil
		},
	)
}

// ValidationOption is an an option for validating the ingestion input data.
// These are defined as constants within this package.
type ValidationOption int8

const (
	// VOUnknown indicates that a ValidationOption was not set.
	VOUnknown ValidationOption = 0
	// SameNumberOfFields indicates that all records ingested must have the same number of fields.
	SameNumberOfFields ValidationOption = 1
	// IgnoreNonDoubleQuotedFields indicates that fields that do not have double quotes should be ignored.
	IgnoreNonDoubleQuotedFields ValidationOption = 2
)

// ValidationImplication is a setting used to indicate what to do when a Validation Policy is violated.
// These are defined as constants within this package.
type ValidationImplication int8

const (
	// FailIngestion indicates that any violation of the ValidationPolicy will cause the entire ingestion to fail.
	FailIngestion ValidationImplication = 0
	// IgnoreFailures indicates that failure of the ValidationPolicy will be ignored.
	IgnoreFailures ValidationImplication = 1
)

// ValPolicy sets a policy for validating data as it is sent for ingestion.
// For more information, see: https://docs.microsoft.com/en-us/azure/kusto/management/data-ingestion/
type ValPolicy struct {
	// Options provides an option that will flag data that does not validate.
	Options ValidationOption `json:"ValidationOptions"`
	// Implications sets what to do when a policy option is violated.
	Implications ValidationImplication `json:"ValidationImplications"`
}

// ValidationPolicy uses a ValPolicy to set our ingestion data validation policy. If not set, no validation policy
// is used.
// For more information, see: https://docs.microsoft.com/en-us/azure/kusto/management/data-ingestion/
func ValidationPolicy(policy ValPolicy) FileOption {
	return propertyOption(
		func(p *properties.All) error {
			b, err := json.Marshal(policy)
			if err != nil {
				return errors.ES(errors.OpUnknown, errors.KInternal, "bug: the ValPolicy provided would not JSON encode").SetNoRetry()
			}

			// You might be asking, what if we are just using blobstore? Well, then this option doesn't matter :)
			p.Ingestion.Additional.ValidationPolicy = string(b)
			return nil
		},
	)
}

// FileFormat can be used to indicate what type of encoding is supported for the file. This is only needed if
// the file extension is not present. A file like: "input.json.gz" or "input.json" does not need this option, while
// "input" would.
func FileFormat(et DataFormat) FileOption {
	return propertyOption(
		func(p *properties.All) error {
			p.Ingestion.Additional.Format = et
			return nil
		},
	)
}

type mapEntry struct {
	Name string
	Kind string
}

// FromFile allows uploading a data file for Kusto from either a local path or a blobstore URI path.
// This method is thread-safe.
func (i *Ingestion) FromFile(ctx context.Context, fPath string, options ...FileOption) error {
	manager, err := getManager(i.client)
	if err != nil {
		return err
	}

	auth, err := manager.AuthContext(ctx)
	if err != nil {
		return err
	}

	props := i.newProp(auth)
	for _, o := range options {
		if propOpt, ok := o.(propertyOption); ok {
			if err := propOpt(&props); err != nil {
				return err
			}
		}
	}

	if props.Ingestion.Additional.IngestionMappingRef != "" {
		if err := i.haveMappingRef(ctx, props.Ingestion.Additional.IngestionMappingRef); err != nil {
			return err
		}
	}

	local, err := filesystem.IsLocalPath(fPath)
	if err != nil {
		return err
	}

	if local {
		return i.fs.Local(ctx, fPath, props)
	}

	return i.fs.Blob(ctx, fPath, 0, props)
}

// FromReader allows uploading a data file for Kusto from an io.Reader. The content is uploaded to Blobstore and
// ingested after all data in the reader is processed. Content should not use compression as the content will be
// compressed with gzip. This method is thread-safe.
func (i *Ingestion) FromReader(ctx context.Context, reader io.Reader, options ...FileOption) error {
	manager, err := getManager(i.client)
	if err != nil {
		return err
	}

	auth, err := manager.AuthContext(ctx)
	if err != nil {
		return err
	}

	props := i.newProp(auth)
	for _, o := range options {
		if propOpt, ok := o.(propertyOption); ok {
			if err := propOpt(&props); err != nil {
				return err
			}
		}
	}

	if props.Ingestion.Additional.Format == DFUnknown {
		return fmt.Errorf("must provide option FileFormat() when using FromReader()")
	}

	if props.Source.DeleteLocalSource {
		return fmt.Errorf("cannot use DeleteLocalSource() with FromReader()")
	}

	if props.Ingestion.Additional.IngestionMappingRef != "" {
		if err := i.haveMappingRef(ctx, props.Ingestion.Additional.IngestionMappingRef); err != nil {
			return err
		}
	}

	return i.fs.Reader(ctx, reader, props)
}

var (
	// ErrTooLarge indicates that the data being passed to a StreamBlock is larger than the maximum StreamBlock size of 4MiB.
	ErrTooLarge = errors.ES(errors.OpIngestStream, errors.KClientArgs, "cannot add data larger than 4MiB")
)

const mib = 1024 * 1024

// Stream takes a payload that is encoded in format with a server stored mappingName, compresses it and uploads it to Kusto.
// payload must be a fully formed entry of format and < 4MiB or this will fail. We currently support
// CSV, TSV, SCSV, SOHSV, PSV, JSON and AVRO. If using JSON or AVRO, you must provide a mappingName that references
// the name of the pre-created ingestion mapping defined on the table. Otherwise mappingName can be an empty string.
// More information can be found here:
// https://docs.microsoft.com/en-us/azure/kusto/management/create-ingestion-mapping-command
// The context object can be used with a timeout or cancel to limit the request time.
func (i *Ingestion) Stream(ctx context.Context, payload []byte, format DataFormat, mappingName string) error {
	if err := i.haveMappingRef(ctx, mappingName); err != nil {
		return err
	}

	c, err := i.getStreamConn()
	if err != nil {
		return err
	}

	buf := conn.BuffPool.Get().(*bytes.Buffer)

	zw := gzip.NewWriter(buf)
	_, err = zw.Write(payload)
	if err != nil {
		return errors.E(errors.OpIngestStream, errors.KClientArgs, err)
	}

	if err := zw.Close(); err != nil {
		return errors.E(errors.OpIngestStream, errors.KClientArgs, err).SetNoRetry()
	}
	if buf.Len() > 4*mib {
		return ErrTooLarge
	}

	return c.Write(ctx, i.db, i.table, buf, format, mappingName)
}

func (i *Ingestion) getStreamConn() (*conn.Conn, error) {
	i.connMu.Lock()
	defer i.connMu.Unlock()

	if i.streamConn != nil {
		return i.streamConn, nil
	}

	sc, err := conn.New(i.client.Endpoint(), i.client.Auth())
	if err != nil {
		return nil, err
	}
	i.streamConn = sc
	return i.streamConn, nil
}

func (i *Ingestion) newProp(auth string) properties.All {
	return properties.All{
		Ingestion: properties.Ingestion{
			DatabaseName:        i.db,
			TableName:           i.table,
			RetainBlobOnSuccess: true,
			Additional: properties.Additional{
				AuthContext: auth,
			},
		},
	}
}

var mapCacheDur = 5 * time.Minute

func (i *Ingestion) haveMappingRef(ctx context.Context, ref string) error {
	i.mappingsMu.Lock()
	defer i.mappingsMu.Unlock()

	if time.Now().Sub(i.lastMappingLookup) < mapCacheDur {
		if _, ok := i.mappings[ref]; ok {
			return nil
		}
		return errors.ES(errors.OpFileIngest, errors.KClientArgs, "could not find a mapping reference for %q", ref)
	}

	iter, err := i.client.Mgmt(ctx, i.db, kusto.NewStmt(".show ingestion mappings"))
	if err != nil {
		return err
	}
	m := map[string]mapEntry{}
	err = iter.Do(
		func(row *table.Row) error {
			mapping := mapEntry{}
			if err := row.ToStruct(&mapping); err != nil {
				return errors.ES(errors.OpFileIngest, errors.KInternal, "problem converting .show ingestion mappings to struct: %s", err)
			}
			m[mapping.Name] = mapping
			return nil
		},
	)
	if err != nil {
		return err
	}
	_, ok := m[ref]
	if !ok {
		return errors.ES(errors.OpFileIngest, errors.KClientArgs, "could not find a mapping reference for %q", ref)
	}
	i.mappings = m
	return nil
}
