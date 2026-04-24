import { useOrganization } from "@clerk/react";
import { useEffect, useState } from "react";
import { api } from "../api/client";

export function Dashboard() {
  const { organization } = useOrganization();
  const [status, setStatus] = useState<string>("checking...");
  const [name, setName] = useState("");
  const [greeting, setGreeting] = useState<string>("");

  useEffect(() => {
    api.GET("/api/health").then(({ data, error }) => {
      setStatus(error ? "error" : (data?.status ?? "unknown"));
    });
  }, []);

  async function sayHello() {
    const { data, error } = await api.GET("/api/ping", {
      params: { query: { name: name || undefined } },
    });
    setGreeting(error ? "error" : (data?.message ?? ""));
  }

  return (
    <>
      <h1 className="text-3xl font-bold tracking-tight">
        {organization?.name ?? "Dashboard"}
      </h1>
      <p className="mt-2 text-sm text-gray-600">
        API health:{" "}
        <strong
          className={
            status === "ok"
              ? "text-green-600"
              : status === "error"
                ? "text-red-600"
                : "text-gray-500"
          }
        >
          {status}
        </strong>
      </p>
      <div className="mt-6 flex gap-2">
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="your name"
          className="flex-1 border border-gray-300 px-3 py-2 text-sm shadow-sm focus:border-indigo-500 focus:outline-none focus:ring-1 focus:ring-indigo-500"
        />
        <button
          onClick={sayHello}
          className="bg-indigo-600 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-indigo-700 focus:outline-none focus:ring-2 focus:ring-indigo-500 focus:ring-offset-2"
        >
          Ping
        </button>
      </div>
      {greeting && (
        <p className="mt-4 bg-gray-50 p-3 text-sm text-gray-800">
          {greeting}
        </p>
      )}
    </>
  );
}
