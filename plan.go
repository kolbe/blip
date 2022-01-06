package blip

// Plan represents different levels of metrics collection.
type Plan struct {
	// Name is the name of the plan (required).
	//
	// When loaded from config.plans.files, Name is the exact name of the config.
	// The first file is the default plan if config.plan.default is not specified.
	//
	// When loaded from a config.plans.table, Name is the name column. The name
	// column cannot be NULL. The plan table is ordered by name (ascending) and
	// the first plan is the default if config.plan.default is not specified.
	//
	// config.plan.adjust.readonly and .active refer to Name.
	Name string

	// Levels are the collection frequencies that constitue the plan (required).
	Levels map[string]Level

	// MonitorId is the optional monitorId column from a plan table.
	//
	// When default plans are loaded from a table (config.plans.table),
	// the talbe is not filtered; all plans in the table are loaded.
	//
	// When a monitor (M) loads plans from a table (config.monitors.M.plans.table),
	// the table is filtered: WHERE monitorId = config.monitors.M.id.
	MonitorId string `yaml:"-"`
}

// Level is one collection frequency in a plan.
type Level struct {
	Name    string            `yaml:"-"`
	Freq    string            `yaml:"freq"`
	Collect map[string]Domain `yaml:"collect"`
}

// Domain is one metric domain for collecting related metrics.
type Domain struct {
	Name    string            `yaml:"-"`
	Options map[string]string `yaml:"options,omitempty"`
	Metrics []string          `yaml:"metrics,omitempty"`
}

// Help represents information about a collector.
type CollectorHelp struct {
	Domain      string
	Description string
	Options     map[string]CollectorHelpOption
}

type CollectorHelpOption struct {
	Name    string
	Desc    string            // describes Name
	Default string            // key in Values
	Values  map[string]string // value => description
}

// --------------------------------------------------------------------------

func (p *Plan) InterpolateEnvVars() {
	for levelName := range p.Levels {
		for domainName := range p.Levels[levelName].Collect {
			for k, v := range p.Levels[levelName].Collect[domainName].Options {
				p.Levels[levelName].Collect[domainName].Options[k] = interpolateEnv(v)
			}
		}
	}
}

func (p *Plan) InterpolateMonitor(mon *ConfigMonitor) {
	for levelName := range p.Levels {
		for domainName := range p.Levels[levelName].Collect {
			for k, v := range p.Levels[levelName].Collect[domainName].Options {
				p.Levels[levelName].Collect[domainName].Options[k] = mon.interpolateMon(v)
			}
		}
	}
}

// --------------------------------------------------------------------------

func InternalLevelPlan() Plan {
	return Plan{
		Name: "blip",
		Levels: map[string]Level{
			"performance": Level{
				Name: "performance",
				Freq: "5s",
				Collect: map[string]Domain{
					"status.global": {
						Name: "status.global",
						Metrics: []string{
							// Key performance indicators (KPIs)
							"queries",
							"threads_running",

							// Transactions per second (TPS)
							"com_begin",
							"com_commit",
							"com_rollback",

							// Read-write access
							"com_select", // read; the rest are writes
							"com_delete",
							"com_delete_multi",
							"com_insert",
							"com_insert_select",
							"com_replace",
							"com_replace_select",
							"com_update",
							"com_update_multi",

							// Storage IOPS
							"innodb_data_reads",
							"innodb_data_writes",

							// Storage throughput (Bytes/s)
							"innodb_data_written",
							"innodb_data_read",

							// Buffer pool efficiency
							"innodb_buffer_pool_read_requests", // logical reads
							"innodb_buffer_pool_reads",         // disk reads (data not in buffer pool)
							"Innodb_buffer_pool_wait_free",     // free page waits

							// Buffer pool usage
							"innodb_buffer_pool_pages_dirty",
							"innodb_buffer_pool_pages_free",
							"innodb_buffer_pool_pages_total",

							// Page flushing
							"innodb_buffer_pool_pages_flushed", // total pages

							// Transaction log throughput (Bytes/s)
							"innodb_os_log_written",
						},
					},
					"innodb": {
						Metrics: []string{
							// Transactions
							"trx_active_transactions", // (G)

							// Row locking
							"lock_timeouts",
							"lock_row_lock_current_waits", // (G)
							"lock_row_lock_waits",
							"lock_row_lock_time",

							// Page flushing
							"buffer_flush_adaptive_total_pages",   //  adaptive flushing
							"buffer_LRU_batch_flush_total_pages",  //  LRU flushing
							"buffer_flush_background_total_pages", //  legacy flushing

							// Transaction log utilization (%)
							"log_lsn_checkpoint_age_total", // checkpoint age
							"log_max_modified_age_async",   // async flush point

							// Transaction log -> storage waits
							"innodb_os_log_pending_writes",
							"innodb_log_waits",

							// History List Length (HLL)
							"trx_rseg_history_len",

							// Deadlocks
							"lock_deadlocks",
						},
					},
				},
			}, // level: performance (5s)

			"additional": Level{
				Name: "additional",
				Freq: "20s",
				Collect: map[string]Domain{
					"status.global": {
						Name: "status.global",
						Metrics: []string{
							// Temp objects
							"created_tmp_disk_tables",
							"created_tmp_tables",
							"created_tmp_files",

							// Threads and connections
							"connections",
							"threads_connected", // (G)
							"max_used_connections",

							// Network throughput
							"bytes_sent",
							"bytes_received",

							// Large data changes cached to disk before binlog
							"binlog_cache_disk_use",

							// Prepared statements
							"prepared_stmt_count", // (G)
							"com_stmt_execute",
							"com_stmt_prepare",

							// Client connection errors
							"aborted_clients",
							"aborted_connects",

							// Bad SELECT: should be zero
							"select_full_join",
							"select_full_range_join",
							"select_range_check",
							"select_scan",

							// Admin and SHOW
							"com_flush",
							"com_kill",
							"com_purge",
							"com_admin_commands",
							"com_show_processlist",
							"com_show_slave_status",
							"com_show_status",
							"com_show_variables",
							"com_show_warnings",
						},
					},
				},
			}, // level: additional (20s)

			"data-size": Level{
				Name: "data-size",
				Freq: "5m",
				Collect: map[string]Domain{
					"size.data": {
						Name: "size.data",
						// All data sizes by default
					},
					"size.binlogs": {
						Name: "size.binlogs",
					},
				},
			}, // level: data-size (5m)

			"sysvars": Level{
				Name: "sysvars",
				Freq: "15m",
				Collect: map[string]Domain{
					"var.global": {
						Name: "var.global",
						Metrics: []string{
							"max_connections",
							"max_prepared_stmt_count",
							"innodb_log_file_size",
							"innodb_max_dirty_pages_pct",
						},
					},
				},
			}, // level: sysvars (15m)

		},
	}
}

func PromPlan() Plan {
	return Plan{
		Name: "mysqld_exporter",
		Levels: map[string]Level{
			"all": Level{
				Name: "all",
				Freq: "", // none, pulled/scaped on demand
				Collect: map[string]Domain{
					"status.global": {
						Name: "status.global",
						Options: map[string]string{
							"all": "yes",
						},
					},
					"var.global": {
						Name: "var.global",
						Options: map[string]string{
							"all": "yes",
						},
					},
					"innodb": {
						Name: "innodb",
						Options: map[string]string{
							"all": "enabled",
						},
					},
				},
			},
		},
	}
}
