// Package matrixprofile computes the matrix profile and matrix profile index of a time series
package matrixprofile

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"

	"gonum.org/v1/gonum/fourier"
)

// MatrixProfile is a struct that tracks the current matrix profile computation
// for a given timeseries of length N and subsequence length of M. The profile
// and the profile index are stored here.
type MatrixProfile struct {
	a        []float64    // query time series
	b        []float64    // timeseries to perform full join with
	aMean    []float64    // sliding mean of a with a window of m each
	aStd     []float64    // sliding standard deviation of a with a window of m each
	bMean    []float64    // sliding mean of b with a window of m each
	bStd     []float64    // sliding standard deviation of b with a window of m each
	bF       []complex128 // holds an existing calculation of the FFT of b timeseries
	n        int          // length of the timeseries
	m        int          // length of a subsequence
	selfJoin bool         // indicates whether a self join is performed with an exclusion zone
	MP       []float64    // matrix profile
	Idx      []int        // matrix profile index
}

// New creates a matrix profile struct with a given timeseries length n and
// subsequence length of m. The first slice, a, is used as the initial
// timeseries to join with the second, b. If b is nil, then the matrix profile
// assumes a self join on the first timeseries.
func New(a, b []float64, m int) (*MatrixProfile, error) {
	if a == nil || len(a) == 0 {
		return nil, fmt.Errorf("first slice is nil or has a length of 0")
	}

	if b != nil && len(b) == 0 {
		return nil, fmt.Errorf("second slice must be nil for self-join operation or have a length greater than 0")
	}

	mp := MatrixProfile{
		a: a,
		m: m,
		n: len(b),
	}
	if b == nil {
		mp.n = len(a)
		mp.b = a
		mp.selfJoin = true
	} else {
		mp.b = b
	}

	if mp.m*2 >= mp.n {
		return nil, fmt.Errorf("subsequence length must be less than half the timeseries")
	}

	if mp.m < 2 {
		return nil, fmt.Errorf("subsequence length must be at least 2")
	}

	if err := mp.initCaches(); err != nil {
		return nil, err
	}

	mp.MP = make([]float64, mp.n-mp.m+1)
	mp.Idx = make([]int, mp.n-m+1)
	for i := 0; i < len(mp.MP); i++ {
		mp.MP[i] = math.Inf(1)
		mp.Idx[i] = math.MaxInt64
	}

	return &mp, nil
}

// initCaches initializes cached data including the timeseries a and b rolling mean
// and standard deviation and full fourier transform of timeseries b
func (mp *MatrixProfile) initCaches() error {
	var err error
	// precompute the mean and standard deviation for each window of size m for all
	// sliding windows across the b timeseries
	mp.bMean, mp.bStd, err = movmeanstd(mp.b, mp.m)
	if err != nil {
		return err
	}

	mp.aMean, mp.aStd, err = movmeanstd(mp.a, mp.m)
	if err != nil {
		return err
	}

	// precompute the fourier transform of the b timeseries since it will
	// be used multiple times while computing the matrix profile
	fft := fourier.NewFFT(mp.n)
	mp.bF = fft.Coefficients(nil, mp.b)

	return nil
}

// crossCorrelate computes the sliding dot product between two slices
// given a query and time series. Uses fast fourier transforms to compute
// the necessary values. Returns the a slice of floats for the cross-correlation
// of the signal q and the mp.b signal. This makes an optimization where the query
// length must be less than half the length of the timeseries, b.
func (mp MatrixProfile) crossCorrelate(q []float64, fft *fourier.FFT) []float64 {
	qpad := make([]float64, mp.n)
	for i := 0; i < len(q); i++ {
		qpad[i] = q[mp.m-i-1]
	}
	qf := fft.Coefficients(nil, qpad)

	// in place multiply the fourier transform of the b time series with
	// the subsequence fourier transform and store in the subsequence fft slice
	for i := 0; i < len(qf); i++ {
		qf[i] = mp.bF[i] * qf[i]
	}

	dot := fft.Sequence(nil, qf)

	for i := 0; i < mp.n-mp.m+1; i++ {
		dot[mp.m-1+i] = dot[mp.m-1+i] / float64(mp.n)
	}
	return dot[mp.m-1:]
}

// mass calculates the Mueen's algorithm for similarity search (MASS)
// between a specified query and timeseries. Writes the euclidean distance
// of the query to every subsequence in mp.b to profile.
func (mp MatrixProfile) mass(q []float64, profile []float64, fft *fourier.FFT) error {
	qnorm, err := zNormalize(q)
	if err != nil {
		return err
	}

	dot := mp.crossCorrelate(qnorm, fft)

	// converting cross correlation value to euclidian distance
	for i := 0; i < len(dot); i++ {
		profile[i] = math.Sqrt(math.Abs(2 * (float64(mp.m) - (dot[i] / mp.bStd[i]))))
	}
	return nil
}

// distanceProfile computes the distance profile between a and b time series.
// If b is set to nil then it assumes a self join and will create an exclusion
// area for trivial nearest neighbors. Writes the euclidean distance between
// the specified subsequence in mp.a with each subsequence in mp.b to profile
func (mp MatrixProfile) distanceProfile(idx int, profile []float64, fft *fourier.FFT) error {
	if idx > len(mp.a)-mp.m {
		return fmt.Errorf("provided index  %d is beyond the length of timeseries %d minus the subsequence length %d", idx, len(mp.a), mp.m)
	}

	if err := mp.mass(mp.a[idx:idx+mp.m], profile, fft); err != nil {
		return err
	}

	// sets the distance in the exclusion zone to +Inf
	if mp.selfJoin {
		applyExclusionZone(profile, idx, mp.m/2)
	}
	return nil
}

// calculateDistanceProfile converts a sliding dot product slice of floats into
// distances and normalizes the output. Writes results back into the profile slice
// of floats representing the distance profile.
func (mp MatrixProfile) calculateDistanceProfile(dot []float64, idx int, profile []float64) error {
	if idx > len(mp.a)-mp.m {
		return fmt.Errorf("provided index %d is beyond the length of timeseries a %d minus the subsequence length %d", idx, len(mp.a), mp.m)
	}

	if len(profile) != len(dot) {
		return fmt.Errorf("profile length, %d, is not the same as the dot product length, %d", len(profile), len(dot))
	}

	// converting cross correlation value to euclidian distance
	for i := 0; i < len(dot); i++ {
		profile[i] = math.Sqrt(2 * float64(mp.m) * math.Abs(1-(dot[i]-float64(mp.m)*mp.bMean[i]*mp.aMean[idx])/(float64(mp.m)*mp.bStd[i]*mp.aStd[idx])))
	}

	if mp.selfJoin {
		// sets the distance in the exclusion zone to +Inf
		applyExclusionZone(profile, idx, mp.m/2)
	}
	return nil
}

// Stmp computes the full matrix profile given two time series as inputs.
// If the second time series is set to nil then a self join on the first
// will be performed. Stores the matrix profile and matrix profile index
// in the struct.
func (mp *MatrixProfile) Stmp() error {
	var err error
	profile := make([]float64, mp.n-mp.m+1)

	fft := fourier.NewFFT(mp.n)
	for i := 0; i < mp.n-mp.m+1; i++ {
		if err = mp.distanceProfile(i, profile, fft); err != nil {
			return err
		}

		for j := 0; j < len(profile); j++ {
			if profile[j] <= mp.MP[j] {
				mp.MP[j] = profile[j]
				mp.Idx[j] = i
			}
		}
	}

	return nil
}

// Stamp uses random ordering to compute the matrix profile. User can specify the
// sample to be anything between 0 and 1 so that the computation early terminates
// and provides the current computed matrix profile. 1 represents the exact matrix
// profile. This should compute far faster at the cost of an approximation of the
// matrix profile. Stores the matrix profile and matrix profile index in the struct.
func (mp *MatrixProfile) Stamp(sample float64, parallelism int) error {
	if sample == 0.0 {
		return fmt.Errorf("must provide a non zero sampling")
	}

	randIdx := rand.Perm(len(mp.a) - mp.m + 1)

	batchSize := (len(mp.a)-mp.m+1)/parallelism + 1
	results := make([]chan mpResult, parallelism)
	for i := 0; i < parallelism; i++ {
		results[i] = make(chan mpResult)
	}

	// go routine to continually check for results on the slice of channels
	// for each batch kicked off. This merges the results of the batched go
	// routines by picking the lowest value in each batch's matrix profile and
	// updating the matrix profile index.
	var err error
	done := make(chan bool)
	go func() {
		err = mp.mergeMPResults(results)
		done <- true
	}()

	// kick off multiple go routines to process a batch of rows returning back
	// the matrix profile for that batch and any error encountered
	var wg sync.WaitGroup
	wg.Add(parallelism)
	for batch := 0; batch < parallelism; batch++ {
		go func(idx int) {
			result := mp.stampBatch(idx, batchSize, sample, randIdx, &wg)
			results[idx] <- result
		}(batch)
	}
	wg.Wait()

	// waits for all results to be read and merged before returning success
	<-done

	return err
}

// stampBatch processes a batch set of rows in a matrix profile calculation
func (mp MatrixProfile) stampBatch(idx, batchSize int, sample float64, randIdx []int, wg *sync.WaitGroup) mpResult {
	defer wg.Done()
	if idx*batchSize+mp.m > len(mp.a) {
		// got an index larger than mp.a so ignore
		return mpResult{}
	}

	// initialize this batch's matrix profile results
	result := mpResult{
		MP:  make([]float64, mp.n-mp.m+1),
		Idx: make([]int, mp.n-mp.m+1),
	}
	for i := 0; i < len(mp.MP); i++ {
		result.MP[i] = math.Inf(1)
		result.Idx[i] = math.MaxInt64
	}

	var err error
	profile := make([]float64, len(result.MP))
	fft := fourier.NewFFT(mp.n)
	for i := 0; i < int(float64(batchSize)*sample); i++ {
		if idx*batchSize+i >= len(randIdx) {
			break
		}
		if err = mp.distanceProfile(randIdx[idx*batchSize+i], profile, fft); err != nil {
			return mpResult{nil, nil, err}
		}
		for j := 0; j < len(profile); j++ {
			if profile[j] <= result.MP[j] {
				result.MP[j] = profile[j]
				result.Idx[j] = randIdx[idx*batchSize+i]
			}
		}
	}
	return result
}

// StampUpdate updates a matrix profile and matrix profile index in place providing streaming
// like behavior.
func (mp *MatrixProfile) StampUpdate(newValues []float64) error {
	var err error

	var profile []float64
	for _, val := range newValues {
		// add to the a and b time series and increment the time series length
		if mp.selfJoin {
			mp.a = append(mp.a, val)
			mp.b = mp.a
		} else {
			mp.b = append(mp.b, val)
		}
		mp.n++

		// increase the size of the Matrix Profile and Index
		mp.MP = append(mp.MP, math.Inf(1))
		mp.Idx = append(mp.Idx, math.MaxInt64)

		if err = mp.initCaches(); err != nil {
			return err
		}

		// only compute the last distance profile
		profile = make([]float64, len(mp.MP))
		fft := fourier.NewFFT(mp.n)
		if err = mp.distanceProfile(len(mp.a)-mp.m, profile, fft); err != nil {
			return err
		}

		minVal := math.Inf(1)
		minIdx := math.MaxInt64
		for j := 0; j < len(profile)-1; j++ {
			if profile[j] <= mp.MP[j] {
				mp.MP[j] = profile[j]
				mp.Idx[j] = mp.n - mp.m
			}
			if profile[j] < minVal {
				minVal = profile[j]
				minIdx = j
			}
		}
		mp.MP[mp.n-mp.m] = minVal
		mp.Idx[mp.n-mp.m] = minIdx
	}
	return nil
}

// mpResult is the output struct from a batch processing for STAMP and STOMP. This struct
// can later be merged together in linear time or with a divide and conquer approach
type mpResult struct {
	MP  []float64
	Idx []int
	Err error
}

// Stomp is an optimization on the STAMP approach reducing the runtime from O(n^2logn)
// down to O(n^2). This is an ordered approach, since the sliding dot product or cross
// correlation can be easily updated for the next sliding window, if the previous window
// dot product is available. This should also greatly reduce the number of memory
// allocations needed to compute an arbitrary timeseries length.
func (mp *MatrixProfile) Stomp(parallelism int) error {
	// save the first dot product of the first row that will be used by all future
	// go routines
	fft := fourier.NewFFT(mp.n)
	cachedDot := mp.crossCorrelate(mp.a[:mp.m], fft)

	batchSize := (len(mp.a)-mp.m+1)/parallelism + 1
	results := make([]chan mpResult, parallelism)
	for i := 0; i < parallelism; i++ {
		results[i] = make(chan mpResult)
	}

	// go routine to continually check for results on the slice of channels
	// for each batch kicked off. This merges the results of the batched go
	// routines by picking the lowest value in each batch's matrix profile and
	// updating the matrix profile index.
	var err error
	done := make(chan bool)
	go func() {
		err = mp.mergeMPResults(results)
		done <- true
	}()

	// kick off multiple go routines to process a batch of rows returning back
	// the matrix profile for that batch and any error encountered
	var wg sync.WaitGroup
	wg.Add(parallelism)
	for batch := 0; batch < parallelism; batch++ {
		go func(idx int) {
			result := mp.stompBatch(idx, batchSize, cachedDot, &wg)
			results[idx] <- result
		}(batch)
	}
	wg.Wait()

	// waits for all results to be read and merged before returning success
	<-done

	return err
}

func (mp *MatrixProfile) mergeMPResults(results []chan mpResult) error {
	var err error

	resultSlice := make([]mpResult, len(results))
	for i := 0; i < len(results); i++ {
		resultSlice[i] = <-results[i]

		// if an error is encountered set the variable so that it can be checked
		// for at the end of processing. Tracks the last error emitted by any
		// batch
		if resultSlice[i].Err != nil {
			err = resultSlice[i].Err
			continue
		}

		// continues to the next loop if the result returned is empty but
		// had no errors
		if resultSlice[i].MP == nil || resultSlice[i].Idx == nil {
			continue
		}
		for j := 0; j < len(resultSlice[i].MP); j++ {
			if resultSlice[i].MP[j] <= mp.MP[j] {
				mp.MP[j] = resultSlice[i].MP[j]
				mp.Idx[j] = resultSlice[i].Idx[j]
			}
		}
	}
	return err
}

// stompBatch processes a batch set of rows in matrix profile calculation. Each batch will comput its first row's dot product and build the subsequent matrix profile and matrix profile index using the stomp iterative algorithm. This also uses the very first row's dot product, cachedDot, to update the very first index of the current row's dot product.
func (mp MatrixProfile) stompBatch(idx, batchSize int, cachedDot []float64, wg *sync.WaitGroup) mpResult {
	defer wg.Done()
	if idx*batchSize+mp.m > len(mp.a) {
		// got an index larger than mp.a so ignore
		return mpResult{}
	}

	// compute for this batch the first row's sliding dot product
	fft := fourier.NewFFT(mp.n)
	dot := mp.crossCorrelate(mp.a[idx*batchSize:idx*batchSize+mp.m], fft)

	profile := make([]float64, len(dot))
	var err error
	if err = mp.calculateDistanceProfile(dot, idx*batchSize, profile); err != nil {
		return mpResult{nil, nil, err}
	}

	// initialize this batch's matrix profile results
	result := mpResult{
		MP:  make([]float64, mp.n-mp.m+1),
		Idx: make([]int, mp.n-mp.m+1),
	}

	copy(result.MP, profile)
	for i := 0; i < len(profile); i++ {
		result.Idx[i] = idx * batchSize
	}

	// iteratively update for this batch each row's matrix profile and matrix
	// profile index
	for i := 1; i < batchSize; i++ {
		if idx*batchSize+i-1 >= len(mp.a) || idx*batchSize+i+mp.m-1 >= len(mp.a) {
			// looking for an index beyond the length of mp.a so ignore and move one
			// with the current processed matrix profile
			break
		}
		for j := mp.n - mp.m; j > 0; j-- {
			dot[j] = dot[j-1] - mp.b[j-1]*mp.a[idx*batchSize+i-1] + mp.b[j+mp.m-1]*mp.a[idx*batchSize+i+mp.m-1]
		}
		dot[0] = cachedDot[idx*batchSize+i]
		if err = mp.calculateDistanceProfile(dot, idx*batchSize+i, profile); err != nil {
			return mpResult{nil, nil, err}
		}

		// element wise min update of the matrix profile and matrix profile index
		for j := 0; j < len(profile); j++ {
			if profile[j] <= result.MP[j] {
				result.MP[j] = profile[j]
				result.Idx[j] = idx*batchSize + i
			}
		}
	}
	return result
}

// MotifGroup stores a list of indices representing a similar motif along
// with the minimum distance that this set of motif composes of.
type MotifGroup struct {
	Idx     []int
	MinDist float64
}

// TopKMotifs will iteratively go through the matrix profile to find the
// top k motifs with a given radius. Only applies to self joins.
func (mp MatrixProfile) TopKMotifs(k int, r float64) ([]MotifGroup, error) {
	if !mp.selfJoin {
		return nil, errors.New("can only find top motifs if a self join is performed")
	}
	var err error

	motifs := make([]MotifGroup, k)

	mpCurrent := make([]float64, len(mp.MP))
	copy(mpCurrent, mp.MP)

	prof := make([]float64, mp.n-mp.m+1)
	for j := 0; j < k; j++ {
		// find minimum distance and index location
		motifDistance := math.Inf(1)
		minIdx := math.MaxInt64
		for i, d := range mpCurrent {
			if d < motifDistance {
				motifDistance = d
				minIdx = i
			}
		}

		if minIdx == math.MaxInt64 {
			// can't find any more motifs so returning what we currently found
			return motifs, nil
		}

		// filter out all indexes that have a distance within r*motifDistance
		motifSet := make(map[int]struct{})
		initialMotif := []int{minIdx, mp.Idx[minIdx]}
		motifSet[minIdx] = struct{}{}
		motifSet[mp.Idx[minIdx]] = struct{}{}

		fft := fourier.NewFFT(mp.n)
		for _, idx := range initialMotif {
			if err = mp.distanceProfile(idx, prof, fft); err != nil {
				return nil, err
			}
			for i, d := range prof {
				if d < motifDistance*r {
					motifSet[i] = struct{}{}
				}
			}
		}

		// store the found motif indexes and create an exclusion zone around
		// each index in the current matrix profile
		motifs[j] = MotifGroup{
			Idx:     make([]int, 0, len(motifSet)),
			MinDist: motifDistance,
		}
		for idx := range motifSet {
			motifs[j].Idx = append(motifs[j].Idx, idx)
			applyExclusionZone(mpCurrent, idx, mp.m/2)
		}

		// sorts the indices in ascending order
		sort.IntSlice(motifs[j].Idx).Sort()
	}

	return motifs, nil
}

// Discords finds the top k time series discords starting indexes from a computed
// matrix profile. Each discovery of a discord will apply an exclusion zone around
// the found index so that new discords can be discovered.
func (mp MatrixProfile) Discords(k int, exclusionZone int) []int {
	mpCurrent := make([]float64, len(mp.MP))
	copy(mpCurrent, mp.MP)

	// if requested k is larger than length of the matrix profile, cap it
	if k > len(mpCurrent) {
		k = len(mpCurrent)
	}

	discords := make([]int, k)
	var maxVal float64
	var maxIdx int
	for i := 0; i < k; i++ {
		maxVal = 0
		maxIdx = math.MaxInt64
		for j, val := range mpCurrent {
			if !math.IsInf(val, 1) && val > maxVal {
				maxVal = val
				maxIdx = j
			}
		}
		discords[i] = maxIdx
		applyExclusionZone(mpCurrent, maxIdx, exclusionZone)
	}
	return discords
}

// Segment finds the the index where there may be a potential timeseries
// change. Returns the index of the potential change, value of the corrected
// arc curve score and the histogram of all the crossings for each index in
// the matrix profile index. This approach is based on the UCR paper on
// segmentation of timeseries using matrix profiles which can be found
// https://www.cs.ucr.edu/%7Eeamonn/Segmentation_ICDM.pdf
func (mp MatrixProfile) Segment() (int, float64, []float64) {
	histo := arcCurve(mp.Idx)

	for i := 0; i < len(histo); i++ {
		if i == 0 || i == len(histo)-1 {
			histo[i] = math.Min(1.0, float64(len(histo)))
		} else {
			histo[i] = math.Min(1.0, histo[i]/iac(float64(i), len(histo)))
		}
	}

	minIdx := math.MaxInt64
	minVal := math.Inf(1)
	for i := 0; i < len(histo); i++ {
		if histo[i] < minVal {
			minIdx = i
			minVal = histo[i]
		}
	}

	return minIdx, float64(minVal), histo
}

// ApplyAV applies an annotation vector to the current matrix profile. Annotation vector
// values must be between 0 and 1.
func (mp *MatrixProfile) ApplyAV(av []float64) ([]float64, error) {
	if len(av) != len(mp.MP) {
		return nil, fmt.Errorf("annotation vector length, %d, does not match matrix profile length, %d", len(av), len(mp.MP))
	}

	// find the maximum matrix profile value
	maxMP := 0.0
	for _, val := range mp.MP {
		if val > maxMP {
			maxMP = val
		}
	}

	// check that all annotation vector values are between 0 and 1
	for idx, val := range av {
		if val < 0.0 || val > 1.0 {
			return nil, fmt.Errorf("got an annotation vector value of %.3f at index %d. must be between 0 and 1", val, idx)
		}
	}

	// applies the matrix profile correction. 1 results in no change to the matrix profile and
	// 0 results in lifting the current matrix profile value by the maximum matrix profile value
	out := make([]float64, len(mp.MP))
	for idx, val := range av {
		out[idx] = mp.MP[idx] + (1-val)*maxMP
	}

	return out, nil
}
