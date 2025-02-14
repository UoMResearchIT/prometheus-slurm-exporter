// SPDX-FileCopyrightText: 2023 Rivos Inc.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/exp/slog"
)

type JobResource struct {
	AllocCpus  float64 `json:"allocated_cpus"`
	AllocNodes map[string]struct {
		Mem float64 `json:"memory"`
	} `json:"allocated_nodes"`
}
type JobMetric struct {
	Account      string      `json:"account"`
	JobId        float64     `json:"job_id"`
	EndTime      float64     `json:"end_time"`
	JobState     string      `json:"job_state"`
	Partition    string      `json:"partition"`
	UserName     string      `json:"user_name"`
	JobResources JobResource `json:"job_resources"`
}

type squeueResponse struct {
	Meta struct {
		SlurmVersion struct {
			Version struct {
				Major int `json:"major"`
				Micro int `json:"micro"`
				Minor int `json:"minor"`
			} `json:"version"`
			Release string `json:"release"`
		} `json:"Slurm"`
	} `json:"meta"`
	Errors []string    `json:"errors"`
	Jobs   []JobMetric `json:"jobs"`
}

func totalAllocMem(resource *JobResource) float64 {
	var allocMem float64
	for _, node := range resource.AllocNodes {
		allocMem += node.Mem
	}
	return allocMem
}

func parseJobMetrics(jsonJobList []byte) ([]JobMetric, error) {
	var squeue squeueResponse
	err := json.Unmarshal(jsonJobList, &squeue)
	if err != nil {
		slog.Error("Unmarshaling node metrics %q", err)
		return nil, err
	}
	return squeue.Jobs, nil
}

type NAbleTime struct{ time.Time }

// report beginning of time in the case of N/A
func (nat *NAbleTime) UnmarshalJSON(data []byte) error {
	var tString string
	if err := json.Unmarshal(data, &tString); err != nil {
		return err
	}
	nullSet := map[string]struct{}{"N/A": {}, "NONE": {}}
	if _, ok := nullSet[tString]; ok {
		nat.Time = time.Time{}
		return nil
	}
	t, err := time.Parse("2006-01-02T15:04:05", tString)
	nat.Time = t
	return err
}

func parseCliFallback(squeue []byte) ([]JobMetric, error) {
	jobMetrics := make([]JobMetric, 0)
	// convert our custom format to the openapi format we expect
	for i, line := range bytes.Split(bytes.Trim(squeue, "\n"), []byte("\n")) {
		var metric struct {
			Account   string    `json:"a"`
			JobId     float64   `json:"id"`
			EndTime   NAbleTime `json:"end_time"`
			JobState  string    `json:"state"`
			Partition string    `json:"p"`
			UserName  string    `json:"u"`
			Cpu       int64     `json:"cpu"`
			Mem       string    `json:"mem"`
		}
		if err := json.Unmarshal(line, &metric); err != nil {
			slog.Error(fmt.Sprintf("squeue fallback parse error: failed on line %d `%s`", i, line))
			return nil, err
		}
		mem, err := MemToFloat(metric.Mem)
		if err != nil {
			return nil, err
		}
		openapiJobMetric := JobMetric{
			Account:   metric.Account,
			JobId:     metric.JobId,
			JobState:  metric.JobState,
			Partition: metric.Partition,
			UserName:  metric.UserName,
			EndTime:   float64(metric.EndTime.Unix()),
			JobResources: JobResource{
				AllocCpus: float64(metric.Cpu),
				AllocNodes: map[string]struct {
					Mem float64 `json:"memory"`
				}{"0": {Mem: mem}},
			},
		}
		jobMetrics = append(jobMetrics, openapiJobMetric)
	}
	return jobMetrics, nil
}

type UserJobMetric struct {
	stateJobCount map[string]float64
	totalJobCount float64
	allocMemory   float64
	allocCpu      float64
}

func parseUserJobMetrics(jobMetrics []JobMetric) map[string]*UserJobMetric {
	userMetricMap := make(map[string]*UserJobMetric)
	for _, jobMetric := range jobMetrics {
		metric, ok := userMetricMap[jobMetric.UserName]
		if !ok {
			metric = &UserJobMetric{
				stateJobCount: make(map[string]float64),
			}
		}
		metric.stateJobCount[jobMetric.JobState]++
		metric.totalJobCount++
		metric.allocMemory += totalAllocMem(&jobMetric.JobResources)
		metric.allocCpu += jobMetric.JobResources.AllocCpus
		userMetricMap[jobMetric.UserName] = metric
	}
	return userMetricMap
}

type AccountMetric struct {
	allocMem      float64
	allocCpu      float64
	stateJobCount map[string]float64
}

func parseAccountMetrics(jobs []JobMetric) map[string]*AccountMetric {
	accountMap := make(map[string]*AccountMetric)
	for _, job := range jobs {
		metric, ok := accountMap[job.Account]
		if !ok {
			metric = &AccountMetric{
				stateJobCount: make(map[string]float64),
			}
			accountMap[job.Account] = metric
		}
		metric.allocCpu += job.JobResources.AllocCpus
		metric.allocMem += totalAllocMem(&job.JobResources)
		metric.stateJobCount[job.JobState]++
	}
	return accountMap
}

type PartitionJobMetric struct {
	partitionState map[string]float64
}

func parsePartitionJobMetrics(jobs []JobMetric) map[string]*PartitionJobMetric {
	partitionMetric := make(map[string]*PartitionJobMetric)
	for _, job := range jobs {
		metric, ok := partitionMetric[job.Partition]
		if !ok {
			metric = &PartitionJobMetric{
				partitionState: make(map[string]float64),
			}
			partitionMetric[job.Partition] = metric
		}
		metric.partitionState[job.JobState]++
	}
	return partitionMetric
}

type JobsCollector struct {
	// collector state
	fetcher      SlurmFetcher
	fallback     bool
	jobAllocCpus *prometheus.Desc
	jobAllocMem  *prometheus.Desc
	// user metrics
	userJobStateTotal *prometheus.Desc
	userJobMemAlloc   *prometheus.Desc
	userJobCpuAlloc   *prometheus.Desc
	// partition
	partitionJobStateTotal *prometheus.Desc
	// account metrics
	accountJobMemAlloc   *prometheus.Desc
	accountJobCpuAlloc   *prometheus.Desc
	accountJobStateTotal *prometheus.Desc
	// exporter metrics
	jobScrapeDuration *prometheus.Desc
	jobScrapeError    prometheus.Counter
}

func NewJobsController(config *Config) *JobsCollector {
	cliOpts := config.cliOpts
	fetcher := config.traceConf.sharedFetcher
	return &JobsCollector{
		fetcher:  fetcher,
		fallback: cliOpts.fallback,
		// individual job metrics
		jobAllocCpus:           prometheus.NewDesc("slurm_job_alloc_cpus", "amount of cpus allocated per job", []string{"jobid"}, nil),
		jobAllocMem:            prometheus.NewDesc("slurm_job_alloc_mem", "amount of mem allocated per job", []string{"jobid"}, nil),
		userJobStateTotal:      prometheus.NewDesc("slurm_user_state_total", "total jobs per state per user", []string{"username", "state"}, nil),
		userJobMemAlloc:        prometheus.NewDesc("slurm_user_mem_alloc", "total mem alloc per user", []string{"username"}, nil),
		userJobCpuAlloc:        prometheus.NewDesc("slurm_user_cpu_alloc", "total cpu alloc per user", []string{"username"}, nil),
		partitionJobStateTotal: prometheus.NewDesc("slurm_partition_job_state_total", "total jobs per partition per state", []string{"partition", "state"}, nil),
		accountJobMemAlloc:     prometheus.NewDesc("slurm_account_mem_alloc", "alloc mem consumed per account", []string{"account"}, nil),
		accountJobCpuAlloc:     prometheus.NewDesc("slurm_account_cpu_alloc", "alloc cpu consumed per account", []string{"account"}, nil),
		accountJobStateTotal:   prometheus.NewDesc("slurm_account_job_state_total", "total jobs per account per job state", []string{"account", "state"}, nil),
		jobScrapeDuration:      prometheus.NewDesc("slurm_job_scrape_duration", fmt.Sprintf("how long the cmd %v took (ms)", cliOpts.squeue), nil, nil),
		jobScrapeError: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "slurm_job_scrape_error",
			Help: "slurm job scrape error",
		}),
	}
}

func (jc *JobsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- jc.jobAllocCpus
	ch <- jc.jobAllocMem
	ch <- jc.userJobStateTotal
	ch <- jc.userJobMemAlloc
	ch <- jc.userJobCpuAlloc
	ch <- jc.partitionJobStateTotal
	ch <- jc.accountJobMemAlloc
	ch <- jc.accountJobCpuAlloc
	ch <- jc.accountJobStateTotal
	ch <- jc.jobScrapeDuration
	ch <- jc.jobScrapeError.Desc()
}

func (jc *JobsCollector) Collect(ch chan<- prometheus.Metric) {
	defer func() {
		ch <- jc.jobScrapeError
	}()
	squeue, err := jc.fetcher.Fetch()
	if err != nil {
		jc.jobScrapeError.Inc()
		slog.Error(fmt.Sprintf("job fetch error %q", err))
		return
	}
	ch <- prometheus.MustNewConstMetric(jc.jobScrapeDuration, prometheus.GaugeValue, float64(jc.fetcher.Duration().Milliseconds()))
	var jobMetrics []JobMetric
	if jc.fallback {
		jobMetrics, err = parseCliFallback(squeue)
	} else {
		jobMetrics, err = parseJobMetrics(squeue)
	}
	if err != nil {
		jc.jobScrapeError.Inc()
		slog.Error(fmt.Sprintf("job failed to parse with %q", err))
		return
	}
	userMetrics := parseUserJobMetrics(jobMetrics)

	for user, metric := range userMetrics {
		if metric.allocCpu > 0 {
			ch <- prometheus.MustNewConstMetric(jc.userJobCpuAlloc, prometheus.GaugeValue, metric.allocCpu, user)
		}
		if metric.allocMemory > 0 {
			ch <- prometheus.MustNewConstMetric(jc.userJobMemAlloc, prometheus.GaugeValue, metric.allocMemory, user)
		}
		for state, count := range metric.stateJobCount {
			if count > 0 {
				ch <- prometheus.MustNewConstMetric(jc.userJobStateTotal, prometheus.GaugeValue, count, user, state)
			}
		}
	}

	accountMetrics := parseAccountMetrics(jobMetrics)
	for account, metric := range accountMetrics {
		ch <- prometheus.MustNewConstMetric(jc.accountJobCpuAlloc, prometheus.GaugeValue, metric.allocCpu, account)
		ch <- prometheus.MustNewConstMetric(jc.accountJobMemAlloc, prometheus.GaugeValue, metric.allocMem, account)
		for state, count := range metric.stateJobCount {
			if count > 0 {
				ch <- prometheus.MustNewConstMetric(jc.accountJobStateTotal, prometheus.GaugeValue, count, account, state)
			}
		}
	}

	partitionJobMetrics := parsePartitionJobMetrics(jobMetrics)
	for partition, stateTotals := range partitionJobMetrics {
		for state, totalJobs := range stateTotals.partitionState {
			ch <- prometheus.MustNewConstMetric(jc.partitionJobStateTotal, prometheus.GaugeValue, totalJobs, partition, state)
		}
	}
}
