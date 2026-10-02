/**
 * Copyright (c) 2025, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

import { fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { ThemeProvider, createTheme } from "@wso2/oxygen-ui";
import type { TraceFilters, TraceOverview } from "@agent-management-platform/types";

// api-client crashes at import time outside a configured app shell, and
// EnvironmentSelector pulls it in; stub both at the module boundary.
vi.mock("@agent-management-platform/api-client", () => ({
  useTraceList: vi.fn(),
  useExportTraces: vi.fn(() => ({ mutateAsync: vi.fn(), isPending: false })),
  useGetOrganization: vi.fn(() => ({
    data: { namespace: "ns" },
    isPending: false,
    isSuccess: true,
  })),
  useGetAgent: vi.fn(() => ({ data: { uuid: "agent-uuid" }, isPending: false, isSuccess: true })),
  useListEnvironments: vi.fn(() => ({
    data: [{ name: "dev" }],
    isPending: false,
    isSuccess: true,
  })),
  isObserverConfigured: vi.fn(() => true),
  useTrack: vi.fn(() => ({ track: vi.fn() })),
  ConsoleAction: { TraceOpened: "trace_opened", DownloadArtifact: "download_artifact" },
  useTrace: vi.fn(() => ({ data: undefined, isLoading: true, isTruncated: false })),
  useTraceScores: vi.fn(() => ({ data: undefined, isLoading: true })),
  useSpanDetail: vi.fn(() => ({ data: undefined, isLoading: false })),
}));
vi.mock("@agent-management-platform/shared-component", () => ({
  EnvironmentSelector: () => null,
}));

import { useTraceList } from "@agent-management-platform/api-client";
import { TracesComponent } from "./Traces.Component";
import { parseTraceFilters, traceFilterChips } from "./traceFilters";

const mockUseTraceList = vi.mocked(useTraceList);

const makeTrace = (traceId: string, errorCount = 0): TraceOverview => ({
  traceId,
  rootSpanId: `${traceId}-root`,
  rootSpanName: `root ${traceId}`,
  rootSpanKind: "agent",
  startTime: "2026-10-01T10:00:00Z",
  endTime: "2026-10-01T10:00:05Z",
  durationInNanos: 5_000_000_000,
  spanCount: 12,
  status: { errorCount },
} as TraceOverview);

// The filtered list keeps only t-err; the unfiltered list holds both.
const ALL = [makeTrace("t-err", 1), makeTrace("t-ok")];
const listCache = new Map<string, unknown>();
const listFor = (filters: TraceFilters = {}) => {
  const key = JSON.stringify(filters);
  if (!listCache.has(key)) {
    const traces = filters.status === "error" ? ALL.filter((t) => t.status?.errorCount) : ALL;
    listCache.set(key, {
      traces,
      totalCount: traces.length,
      fetchedRange: { startTime: "2026-10-01T09:00:00Z", endTime: "2026-10-01T11:00:00Z" },
    });
  }
  return listCache.get(key);
};

const lastFilters = () => mockUseTraceList.mock.lastCall?.[9]?.filters;

function SearchProbe() {
  return <div data-testid="search">{useLocation().search}</div>;
}
const currentParams = () => new URLSearchParams(screen.getByTestId("search").textContent ?? "");

const renderPage = (search = "") =>
  render(
    <ThemeProvider theme={createTheme()}>
      <MemoryRouter initialEntries={[`/org/o/project/p/agents/a/environment/dev/traces${search}`]}>
        <Routes>
          <Route
            path="/org/:orgId/project/:projectId/agents/:agentId/environment/:envId/traces"
            element={
              <>
                <TracesComponent />
                <SearchProbe />
              </>
            }
          />
        </Routes>
      </MemoryRouter>
    </ThemeProvider>,
  );

// hidden: an open drawer marks the page behind it aria-hidden.
const pickOption = (selectName: string, optionName: string) => {
  fireEvent.mouseDown(screen.getByRole("combobox", { name: selectName, hidden: true }));
  fireEvent.click(within(screen.getByRole("listbox")).getByRole("option", { name: optionName }));
};

describe("trace filter URL parsing", () => {
  it("keeps valid params and drops ones the API would reject", () => {
    expect(
      parseTraceFilters(
        new URLSearchParams(
          "status=error&minDurationMs=5000&minTokens=5k&minSpanCount=-1&model=%20gpt-4o%20&conversationId=",
        ),
      ),
    ).toEqual({ status: "error", minDurationMs: 5000, model: "gpt-4o" });
    expect(parseTraceFilters(new URLSearchParams("status=failed&minTokens=0"))).toEqual({
      minTokens: 0,
    });
  });

  it("labels chips for people", () => {
    expect(
      traceFilterChips({ minDurationMs: 5000, minTokens: 10000, minSpanCount: 20 }).map(
        (c) => c.label,
      ),
    ).toEqual(["Latency ≥ 5s", "Tokens ≥ 10k", "Steps ≥ 20"]);
  });
});

describe("TracesComponent filters", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    listCache.clear();
    mockUseTraceList.mockImplementation((...args) => ({
      data: listFor(args[9]?.filters),
      isLoading: false,
      refetch: vi.fn(),
      isRefetching: false,
      loadOlder: vi.fn(),
      loadNewer: vi.fn(),
      isLoadingOlder: false,
      isLoadingNewer: false,
      hasOlder: false,
    }) as unknown as ReturnType<typeof useTraceList>);
  });

  it("reproduces a pasted URL's filters in the request and the chips", () => {
    renderPage("?timeRange=1h&status=error&minDurationMs=5000&model=gpt-4o&minTokens=abc");

    expect(lastFilters()).toEqual({ status: "error", minDurationMs: 5000, model: "gpt-4o" });
    expect(screen.getByText("Status: Error", { selector: ".MuiChip-label" })).toBeInTheDocument();
    expect(screen.getByText("Latency ≥ 5s", { selector: ".MuiChip-label" })).toBeInTheDocument();
    expect(screen.getByText("Model: gpt-4o", { selector: ".MuiChip-label" })).toBeInTheDocument();
    expect(screen.queryByText(/^Tokens ≥/, { selector: ".MuiChip-label" })).not.toBeInTheDocument();
  });

  it("writes a picked filter to the URL under the API param name", () => {
    renderPage("?timeRange=1h&limit=20");

    pickOption("Latency ≥", "10s");
    pickOption("Tokens ≥", "5k");

    const params = currentParams();
    expect(params.get("minDurationMs")).toBe("10000");
    expect(params.get("minTokens")).toBe("5000");
    expect(params.get("timeRange")).toBe("1h");
    expect(params.get("limit")).toBe("20");
    expect(lastFilters()).toEqual({ minDurationMs: 10000, minTokens: 5000 });
  });

  it("commits text filters on Enter or blur, not per keystroke", () => {
    renderPage();

    const model = screen.getByRole("textbox", { name: "Model" });
    fireEvent.change(model, { target: { value: "gpt-4o" } });
    expect(currentParams().get("model")).toBeNull();
    fireEvent.keyDown(model, { key: "Enter" });
    expect(currentParams().get("model")).toBe("gpt-4o");

    const conversation = screen.getByRole("textbox", { name: "Conversation ID" });
    fireEvent.change(conversation, { target: { value: " conv-1 " } });
    fireEvent.blur(conversation);
    expect(currentParams().get("conversationId")).toBe("conv-1");
  });

  it("removes only the chip's filter, and Clear all removes the rest", () => {
    renderPage("?timeRange=1h&status=error&minSpanCount=20&model=gpt-4o");

    fireEvent.click(screen.getByLabelText("Remove Steps ≥ 20"));
    let params = currentParams();
    expect(params.get("minSpanCount")).toBeNull();
    expect(params.get("status")).toBe("error");
    expect(params.get("model")).toBe("gpt-4o");

    fireEvent.click(screen.getByRole("button", { name: "Clear all" }));
    params = currentParams();
    expect(params.get("status")).toBeNull();
    expect(params.get("model")).toBeNull();
    expect(params.get("timeRange")).toBe("1h");
    expect(lastFilters()).toEqual({});
  });

  it("hides Clear all with a single active filter", () => {
    renderPage("?status=ok");
    expect(screen.getByText("Status: OK", { selector: ".MuiChip-label" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Clear all" })).not.toBeInTheDocument();
  });

  it("keeps the drawer open when its trace is still in the filtered list", () => {
    renderPage("?selectedTrace=t-err");
    pickOption("Status", "Error");
    expect(currentParams().get("selectedTrace")).toBe("t-err");
  });

  it("closes the drawer when a filter drops its trace", () => {
    renderPage("?selectedTrace=t-ok");
    pickOption("Status", "Error");
    expect(currentParams().get("status")).toBe("error");
    expect(currentParams().get("selectedTrace")).toBeNull();
  });
});
