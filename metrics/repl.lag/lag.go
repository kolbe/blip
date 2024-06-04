// Copyright 2024 Block, Inc.

package repllag

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/cashapp/blip"
	"github.com/cashapp/blip/heartbeat"
	"github.com/cashapp/blip/sqlutil"
)

const (
	DOMAIN = "repl.lag"

	OPT_HEARTBEAT_SOURCE_ID                = "source-id"
	OPT_HEARTBEAT_SOURCE_ROLE              = "source-role"
	OPT_HEARTBEAT_TABLE                    = "table"
	OPT_WRITER                             = "writer"
	OPT_REPL_CHECK                         = "repl-check"
	OPT_REPORT_NO_HEARTBEAT                = "report-no-heartbeat"
	OPT_REPORT_NOT_A_REPLICA               = "report-not-a-replica"
	OPT_RENAME_DEFAULT_REPLICATION_CHANNEL = "rename-default-replication-channel"
	OPT_NETWORK_LATENCY                    = "network-latency"

	LAG_WRITER_BLIP = "blip"
	LAG_WRITER_PFS  = "pfs"

	// MySQL8LagQuery is the query to calculate approximate lag
	// from replication worker stats in performance schema
	// This is only available in MySQL 8 (and above) and when performance schema is enabled
	MySQL8LagQuery = `WITH applier_latency AS (
 SELECT TIMESTAMPDIFF(MICROSECOND, LAST_APPLIED_TRANSACTION_ORIGINAL_COMMIT_TIMESTAMP, LAST_APPLIED_TRANSACTION_END_APPLY_TIMESTAMP)/1000 as applier_latency_ms
 FROM performance_schema.replication_applier_status_by_worker ORDER BY LAST_APPLIED_TRANSACTION_END_APPLY_TIMESTAMP DESC LIMIT 1
), queue_latency AS (
 SELECT MIN(
 CASE
  WHEN
   LAST_QUEUED_TRANSACTION = 'ANONYMOUS' OR
   LAST_APPLIED_TRANSACTION = 'ANONYMOUS' OR
   GTID_SUBTRACT(LAST_QUEUED_TRANSACTION, LAST_APPLIED_TRANSACTION) = ''
  THEN 0
   ELSE
   TIMESTAMPDIFF(MICROSECOND, LAST_APPLIED_TRANSACTION_IMMEDIATE_COMMIT_TIMESTAMP, NOW(3))/1000
 END
) AS queue_latency_ms,
IF(MIN(TIMESTAMPDIFF(MINUTE, LAST_QUEUED_TRANSACTION_ORIGINAL_COMMIT_TIMESTAMP, NOW()))>1,'IDLE','ACTIVE') as queue_status
FROM performance_schema.replication_applier_status_by_worker w
JOIN performance_schema.replication_connection_status s ON s.channel_name = w.channel_name
)
SELECT IF(queue_status='IDLE',0,GREATEST(applier_latency_ms, queue_latency_ms)) as lagMs FROM applier_latency, queue_latency;`

	defaultChannelName = "default"
)

type Lag struct {
	db                              *sql.DB
	lagReader                       heartbeat.Reader
	lagWriterIn                     map[string]string
	dropNoHeartbeat                 map[string]bool
	dropNotAReplica                 map[string]bool
	renameDefaultReplicationChannel map[string]bool
	replCheck                       string
}

var _ blip.Collector = &Lag{}

func NewLag(db *sql.DB) *Lag {
	return &Lag{
		db:                              db,
		lagWriterIn:                     map[string]string{},
		dropNoHeartbeat:                 map[string]bool{},
		dropNotAReplica:                 map[string]bool{},
		renameDefaultReplicationChannel: map[string]bool{},
	}
}

func (c *Lag) Domain() string {
	return DOMAIN
}

func (c *Lag) Help() blip.CollectorHelp {
	return blip.CollectorHelp{
		Domain:      DOMAIN,
		Description: "Replication lag",
		Options: map[string]blip.CollectorHelpOption{
			OPT_WRITER: {
				Name:    OPT_WRITER,
				Desc:    "How to collect Lag",
				Default: "auto",
				Values: map[string]string{
					"auto": "Auto-determine best lag writer",
					"blip": "Native Blip heartbeat replication lag",
					"pfs":  "Performance Schema",
					///"legacy": "Second_Behind_Slave|Replica from SHOW SHOW|REPLICA STATUS",
				},
			},
			OPT_HEARTBEAT_TABLE: {
				Name:    OPT_HEARTBEAT_TABLE,
				Desc:    "Heartbeat table",
				Default: blip.DEFAULT_HEARTBEAT_TABLE,
			},
			OPT_HEARTBEAT_SOURCE_ID: {
				Name: OPT_HEARTBEAT_SOURCE_ID,
				Desc: "Source ID as reported by heartbeat writer; mutually exclusive with " + OPT_HEARTBEAT_SOURCE_ROLE,
			},
			OPT_HEARTBEAT_SOURCE_ROLE: {
				Name: OPT_HEARTBEAT_SOURCE_ROLE,
				Desc: "Source role as reported by heartbeat writer; mutually exclusive with " + OPT_HEARTBEAT_SOURCE_ID,
			},
			OPT_REPL_CHECK: {
				Name: OPT_REPL_CHECK,
				Desc: "MySQL global variable (without @@) to check if instance is a replica",
			},
			OPT_REPORT_NO_HEARTBEAT: {
				Name:    OPT_REPORT_NO_HEARTBEAT,
				Desc:    "Report no heartbeat as -1",
				Default: "no",
				Values: map[string]string{
					"yes": "Enabled: report no heartbeat as repl.lag.current = -1",
					"no":  "Disabled: drop repl.lag.current if no heartbeat",
				},
			},
			OPT_REPORT_NOT_A_REPLICA: {
				Name:    OPT_REPORT_NOT_A_REPLICA,
				Desc:    "Report not a replica as -1",
				Default: "no",
				Values: map[string]string{
					"yes": "Enabled: report not a replica repl.lag.current = -1",
					"no":  "Disabled: drop repl.lag.current if not a replica",
				},
			},
			OPT_RENAME_DEFAULT_REPLICATION_CHANNEL: {
				Name:    OPT_RENAME_DEFAULT_REPLICATION_CHANNEL,
				Desc:    "Rename default replication channel to 'default'",
				Default: "no",
				Values: map[string]string{
					"yes": "Enabled: rename default replication channel to 'default'",
					"no":  "Disabled: do not rename default replication channel",
				},
			},
			OPT_NETWORK_LATENCY: {
				Name:    OPT_NETWORK_LATENCY,
				Desc:    "Network latency (milliseconds)",
				Default: "50",
			},
		},
		Metrics: []blip.CollectorMetric{
			{
				Name: "current",
				Type: blip.GAUGE,
				Desc: "Current replication lag (milliseconds)",
			},
		},
	}
}

// Prepare prepares one lag collector for all levels in the plan. Lag can
// (and probably will be) collected at multiple levels, but this domain can
// be configured at only one level. For example, it's not possible to collect
// lag from a Blip heartbeat and from Performance Schema. And since this
// domain collects only one metric (repl.lag.current), there's no need to
// collect different metrics at different frequencies.
func (c *Lag) Prepare(ctx context.Context, plan blip.Plan) (func(), error) {
	configured := ""   // set after first level to its writer value
	var cleanup func() // Blip heartbeat reader func, else nil
	var err error

LEVEL:
	for levelName, level := range plan.Levels {
		dom, ok := level.Collect[DOMAIN]
		if !ok {
			continue LEVEL // not collected in this level
		}

		writer := dom.Options[OPT_WRITER]

		// Already configured? If yes and same writer, that's ok and expected
		// (lag collected at multiple levels). But if writer is different, that's
		// and error.
		if configured != "" {
			if configured != writer {
				return nil, fmt.Errorf("different writer configuration: %s != %s", configured, writer)
			}
			c.lagWriterIn[levelName] = writer // collect at this level
			continue LEVEL
		}

		blip.Debug("repl.lag: config from level %s", levelName)
		switch writer {
		case LAG_WRITER_PFS:
			// Try collecting, discard metrics
			if _, err = c.collectPFSv2(ctx, levelName); err != nil {
				return nil, err
			}
		case LAG_WRITER_BLIP:
			cleanup, err = c.prepareBlip(levelName, plan.MonitorId, plan.Name, dom.Options)
			if err != nil {
				return nil, err
			}
		case "auto", "": // default
			// Try PFS first
			if _, err = c.collectPFSv2(ctx, levelName); err == nil {
				blip.Debug("repl.lag auto-detected PFS")
				writer = LAG_WRITER_PFS
			} else {
				// then Blip HeartBeat
				if cleanup, err = c.prepareBlip(levelName, plan.MonitorId, plan.Name, dom.Options); err == nil {
					blip.Debug("repl.lag auto-detected Blip heartbeat")
					writer = LAG_WRITER_BLIP
				} else {
					return nil, fmt.Errorf("failed to auto-detect source, set %s manually", OPT_WRITER)
				}
			}
		default:
			return nil, fmt.Errorf("invalid lag writer: %q; valid values: auto, pfs, blip", writer)
		}

		c.lagWriterIn[levelName] = writer // collect at this level

		c.dropNotAReplica[levelName] = !blip.Bool(dom.Options[OPT_REPORT_NOT_A_REPLICA])
		c.renameDefaultReplicationChannel[levelName] = !blip.Bool(dom.Options[OPT_RENAME_DEFAULT_REPLICATION_CHANNEL])
		c.replCheck = sqlutil.CleanObjectName(dom.Options[OPT_REPL_CHECK]) // @todo sanitize better
	}

	return cleanup, nil
}

func (c *Lag) Collect(ctx context.Context, levelName string) ([]blip.MetricValue, error) {
	switch c.lagWriterIn[levelName] {
	case LAG_WRITER_BLIP:
		return c.collectBlip(ctx, levelName)
	case LAG_WRITER_PFS:
		return c.collectPFSv2(ctx, levelName)
	}

	panic(fmt.Sprintf("invalid lag writer in Collect %q in level %q. All levels: %v", c.lagWriterIn[levelName], levelName, c.lagWriterIn))
}

// //////////////////////////////////////////////////////////////////////////
// Internal methods
// //////////////////////////////////////////////////////////////////////////

func (c *Lag) prepareBlip(levelName string, monitorID string, planName string, options map[string]string) (func(), error) {
	if c.lagReader != nil {
		return nil, nil
	}

	c.dropNoHeartbeat[levelName] = !blip.Bool(options[OPT_REPORT_NO_HEARTBEAT])

	table := options[OPT_HEARTBEAT_TABLE]
	if table == "" {
		table = blip.DEFAULT_HEARTBEAT_TABLE
	}
	netLatency := 50 * time.Millisecond
	if s, ok := options[OPT_NETWORK_LATENCY]; ok {
		n, err := strconv.Atoi(s)
		if err != nil {
			blip.Debug("%s: invalid network-latency: %s: %s (ignoring; using default 50 ms)", monitorID, s, err)
		} else {
			netLatency = time.Duration(n) * time.Millisecond
		}
	}
	// Only 1 reader per plan
	c.lagReader = heartbeat.NewBlipReader(heartbeat.BlipReaderArgs{
		MonitorId:  monitorID,
		DB:         c.db,
		Table:      table,
		SourceId:   options[OPT_HEARTBEAT_SOURCE_ID],
		SourceRole: options[OPT_HEARTBEAT_SOURCE_ROLE],
		ReplCheck:  c.replCheck,
		Waiter: heartbeat.SlowFastWaiter{
			MonitorId:      monitorID,
			NetworkLatency: netLatency,
		},
	})
	go c.lagReader.Start()
	blip.Debug("%s: started reader: %s/%s (network latency: %s)", monitorID, planName, levelName, netLatency)
	c.lagWriterIn[levelName] = LAG_WRITER_BLIP
	var cleanup func()
	cleanup = func() {
		blip.Debug("%s: stopping reader", monitorID)
		c.lagReader.Stop()
	}
	return cleanup, nil
}

func (c *Lag) collectBlip(ctx context.Context, levelName string) ([]blip.MetricValue, error) {
	lag, err := c.lagReader.Lag(ctx)
	if err != nil {
		return nil, err
	}
	if !lag.Replica {
		if c.dropNotAReplica[levelName] {
			return nil, nil
		}
	} else if lag.Milliseconds == -1 && c.dropNoHeartbeat[levelName] {
		return nil, nil
	}
	m := blip.MetricValue{
		Name:  "current",
		Type:  blip.GAUGE,
		Value: float64(lag.Milliseconds),
		Meta:  map[string]string{"source": lag.SourceId},
	}
	return []blip.MetricValue{m}, nil
}

func (c *Lag) collectPFS(ctx context.Context, levelName string) ([]blip.MetricValue, error) {
	var defaultLag []blip.MetricValue
	if c.dropNotAReplica[levelName] {
		defaultLag = nil
	} else {
		// send -1 for lag
		m := blip.MetricValue{
			Name:  "current",
			Type:  blip.GAUGE,
			Value: float64(-1),
		}
		defaultLag = []blip.MetricValue{m}
	}

	// if isReplCheck is supplied, check if it's a replica
	isRepl := 1
	if c.replCheck != "" {
		query := "SELECT @@" + c.replCheck
		if err := c.db.QueryRowContext(ctx, query).Scan(&isRepl); err != nil {
			return nil, fmt.Errorf("checking if instance is replica failed, please check value of %s. Err: %s", OPT_REPL_CHECK, err.Error())
		}
	}

	if isRepl == 0 {
		return defaultLag, nil
	}

	// instance is a replica or replCheck is not set
	var lagValue sql.NullString
	if err := c.db.QueryRowContext(ctx, MySQL8LagQuery).Scan(&lagValue); err != nil {
		return nil, fmt.Errorf("could not check replication lag, check that this is a MySQL 8.0 replica, and that performance_schema is enabled. Err: %s", err.Error())
	}
	if !lagValue.Valid {
		// required performance schema table exists, otherwise the query would have returned error

		// if replCheck is empty, we can assume based on the query that it's not a replica and return nil or -1
		if c.replCheck == "" {
			return defaultLag, nil
		} else {
			// it's a replica, so lagValue should be valid, but it's not so raise error
			return nil, fmt.Errorf("cannot determine replica lag because performance_schema.replication_applier_status_by_worker returned an invalid value: %q (expected a positive integer value)", lagValue.String)
		}
	}

	f, ok := sqlutil.Float64(lagValue.String)
	if !ok {
		return nil, fmt.Errorf("couldn't convert replica lag from performance schema into float. Lag: %s", lagValue.String)
	}
	m := blip.MetricValue{
		Name:  "current",
		Type:  blip.GAUGE,
		Value: f,
	}
	return []blip.MetricValue{m}, nil
}
