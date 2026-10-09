/**
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

import React from "react";
import { FormControl, MenuItem, Select, Tooltip } from "@wso2/oxygen-ui";

const ANY = "";

export interface ToolFailedSelectProps {
  value?: boolean;
  onChange: (value?: true) => void;
}

/** Any or Yes; Yes keeps only traces with a failed tool. */
export const ToolFailedSelect: React.FC<ToolFailedSelectProps> = ({ value, onChange }) => (
  <FormControl size="small" sx={{ minWidth: 140 }}>
    <Select
      value={value ? "true" : ANY}
      displayEmpty
      inputProps={{ "aria-label": "Tool failed" }}
      onChange={(e) => onChange(e.target.value === "true" ? true : undefined)}
      renderValue={(v) => `Tool failed: ${v === "true" ? "Yes" : "Any"}`}
    >
      <MenuItem value={ANY}>Any</MenuItem>
      <MenuItem value="true">Yes</MenuItem>
    </Select>
  </FormControl>
);

export interface EvaluatorSelectProps {
  value?: string;
  // Evaluator names to offer; undefined until loaded.
  options?: string[];
  loading?: boolean;
  disabled?: boolean;
  // Called on each open, so the options can load on first use.
  onOpen?: () => void;
  onChange: (value?: string) => void;
}

/** Evaluator select; a value from a pasted URL shows before the options load. */
export const EvaluatorSelect: React.FC<EvaluatorSelectProps> = ({
  value,
  options,
  loading = false,
  disabled = false,
  onOpen,
  onChange,
}) => {
  const names = value && !options?.includes(value) ? [value, ...(options ?? [])] : (options ?? []);
  return (
    <Tooltip title={disabled ? "Pick a score bound to filter by evaluator" : ""}>
      <span>
        <FormControl size="small" sx={{ minWidth: 140 }} disabled={disabled}>
          <Select
            value={value ?? ANY}
            displayEmpty
            inputProps={{ "aria-label": "Evaluator" }}
            onOpen={onOpen}
            onChange={(e) => {
              const raw = e.target.value as string;
              onChange(raw === ANY ? undefined : raw);
            }}
            renderValue={(v) => `Evaluator: ${v === ANY ? "Any" : v}`}
          >
            <MenuItem value={ANY}>Any</MenuItem>
            {names.map((name) => (
              <MenuItem key={name} value={name}>
                {name}
              </MenuItem>
            ))}
            {loading && <MenuItem disabled>Loading evaluators…</MenuItem>}
            {!loading && options?.length === 0 && <MenuItem disabled>No evaluators</MenuItem>}
          </Select>
        </FormControl>
      </span>
    </Tooltip>
  );
};
