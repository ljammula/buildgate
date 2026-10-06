import { useState } from "react";
import { useNavigate } from "react-router";

import { useCreateRequest } from "@/api/requestQueries";
import { getOperatorName } from "@/platform/operatorIdentity";
import { requestPath } from "@/routes/paths";

export interface NewRequestValues {
  readonly workspace: string;
  readonly text: string;
  readonly verifyCommand: string;
  readonly fullSuiteCommand: string;
  readonly preflightProfile: string;
  readonly draftOracles: boolean;
}

const emptyValues: NewRequestValues = {
  workspace: "",
  text: "",
  verifyCommand: "",
  fullSuiteCommand: "",
  preflightProfile: "",
  draftOracles: false,
};

export interface NewRequestErrors {
  readonly workspace: string | null;
  readonly text: string | null;
}

/** The two required fields; every Advanced field is an optional override of a server default. */
function validateNewRequest(values: NewRequestValues): NewRequestErrors {
  return {
    workspace: values.workspace.trim() === "" ? "Workspace path is required" : null,
    text: values.text.trim() === "" ? "Request is required" : null,
  };
}

/** The form's values, validation, operator-name prompt and submission. */
export function useNewRequestForm() {
  const navigate = useNavigate();
  const create = useCreateRequest();
  const [values, setValues] = useState<NewRequestValues>(emptyValues);
  const [attempted, setAttempted] = useState(false);
  const [askName, setAskName] = useState(false);

  const errors = attempted ? validateNewRequest(values) : { workspace: null, text: null };

  const send = (by: string) => {
    create.mutate(
      {
        workspace: values.workspace.trim(),
        text: values.text.trim(),
        verifyCommand: values.verifyCommand.trim(),
        fullSuiteCommand: values.fullSuiteCommand.trim(),
        preflightProfile: values.preflightProfile.trim(),
        draftOracles: values.draftOracles,
        by,
      },
      { onSuccess: (request) => void navigate(requestPath(request.id), { replace: true }) },
    );
  };

  const submit = () => {
    setAttempted(true);
    const found = validateNewRequest(values);
    if (found.workspace !== null || found.text !== null) return;
    const stored = getOperatorName();
    if (stored !== null && stored !== "") send(stored);
    else setAskName(true);
  };

  return {
    values,
    set: (patch: Partial<NewRequestValues>) => {
      setValues((current) => ({ ...current, ...patch }));
    },
    errors,
    submit,
    submitting: create.isPending,
    error: create.error,
    askName,
    confirmName: (name: string) => {
      setAskName(false);
      send(name);
    },
    cancelName: (open: boolean) => {
      if (!open) setAskName(false);
    },
  };
}
