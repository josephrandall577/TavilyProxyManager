---
name: tavily-proxy-search
description: Use the configured tavily-proxy MCP for focused, source-backed web search with minimal calls. Trigger for current or unstable facts, external verification, source citations, news, documentation lookup, URL extraction, site research, comparisons, and decision-ready research. Prefer this skill whenever a task requires internet access unless the user specifies another source or forbids web access.
---

# Tavily Proxy Search

Use Tavily only when local files, supplied material, and stable knowledge cannot answer the task. Optimize for sufficient evidence, not maximum result volume.

## Workflow

1. Define one core question, required facts, freshness window, trusted source types, and stop condition.
2. Reuse user-provided facts and URLs. Do not search them again unless verification is required.
3. Select one route from the tool guide below. Do not run Search and Research workflows in parallel.
4. Make the smallest useful call, filter results by authority and relevance, then stop when evidence is sufficient.
5. Allow one targeted follow-up only for a named evidence gap or source conflict. Never repeat a query with synonyms alone.

## Call Budget

- Simple fact, version, date, or official page: one `tavily_search` call.
- Normal sourced answer: at most two calls, usually one Search plus one batch Extract.
- Complex report: one `tavily_research` call, or at most three Search/Extract calls; choose one route.
- Exceed these limits only when the user explicitly requests exhaustive coverage. Otherwise report remaining uncertainty.
- Use `tavily_usage` only for quota questions or actual quota failures.

## Tool Guide

- `tavily_search`: default when sources are unknown or information is current.
- `tavily_extract`: URL is known, or Search found a high-value page whose snippet is insufficient. Batch 1-3 selected URLs in one call.
- `tavily_map`: site-wide task where relevant paths are unknown.
- `tavily_crawl`: answer truly spans several pages of one site and Search/Extract is insufficient.
- `tavily_research`: finished cited synthesis, complex comparison, or decision-ready report. Use `mini` for narrow work; `pro` only for explicitly comprehensive multi-domain research; `auto` only when complexity is unclear.

## Search Rules

- Keep `query` below 400 characters. Use one intent with exact entity, fact, version, date, and geography where relevant. Remove output-format instructions.
- Merge closely related aspects. Split independent topics into at most two queries; use Research when more research axes are required.
- Default to `auto_parameters=false`, `max_results=5`, `include_answer=false`, `include_raw_content=false`, and images off.
- Use `search_depth=basic` for quick facts and official-page discovery.
- Use one `search_depth=advanced` call with `chunks_per_source=3` for detailed comparisons or high-confidence source discovery. Prefer it over several broad Basic calls.
- Use `topic=news` plus a date filter for news; use `topic=finance` for financial retrieval; otherwise use `general`.
- Keep `include_domains` short and use it for known authoritative sources. Use quoted phrases and `exact_match=true` only when verbatim matching is essential.

## Extraction And Site Rules

- If Search snippets support the answer, do not Extract.
- Extract only deduplicated, authoritative, complementary URLs. Supply a focused `query` and `chunks_per_source=2` or `3`.
- Default to `extract_depth=basic`; use `advanced` only for complex tables, dynamic pages, structured content, or Basic failure.
- Do not extract the same canonical URL twice.
- If the target page is known, Extract directly without Map.
- Crawl narrowly: default `max_depth=1`, `limit<=10`, `allow_external=false`, plus path filters or precise instructions.

## Evidence And Stop Rules

- Rank sources: official/primary sources first, then authoritative professional reporting, then reliable secondary analysis. Community content supports experience claims only.
- One current official source is enough for a direct specification claim. Important, disputed, or high-risk conclusions should use two independent reliable sources when available.
- For changing facts, verify publication date, event date, version, and jurisdiction as applicable.
- Deduplicate canonical URLs and syndicated copies. Distinguish source statements from inference.
- Put direct page links beside supported claims. Never cite a search-results page.
- Stop when the core question is covered, key claims have sufficient evidence, results become repetitive, or the call budget is reached.

## Failure Handling

- Retry once only when a corrected parameter or transient failure justifies it. Do not repeat an unchanged failed call.
- A configured MCP is not necessarily exposed to the current task. Call tools only when `tavily_*` tools are actually available.
- If tools are missing, authentication fails, or the service remains unavailable, state that briefly and use another web source unless the user required Tavily exclusively.
- Apply the same call budget and source-quality rules to any fallback search.
