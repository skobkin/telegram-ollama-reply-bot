// Package extractor turns fetched web pages into articles.
//
// The pipeline fetches once through the shared SSRF-policy fetcher
// (internal/content/fetch) and runs configurable extraction backends over
// the fetched page: the first result passing the quality gate wins, with the
// best-scoring non-empty result as a rescue. The winning content is rendered
// both as plain text (compatibility) and Markdown (structure-preserving,
// the preferred LLM input).
package extractor
