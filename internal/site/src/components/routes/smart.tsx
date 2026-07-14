import { useEffect } from "react"
import SmartTable from "@/components/routes/system/smart-table"
import { ActiveAlerts } from "@/components/active-alerts"
import { FooterRepoLink } from "@/components/footer-repo-link"
import { pageTitle } from "@/lib/build-info"

export default function Smart() {
	useEffect(() => {
		document.title = pageTitle("S.M.A.R.T.")
	}, [])

	return (
		<>
			<div className="grid gap-4">
				<ActiveAlerts />
				<SmartTable />
			</div>
			<FooterRepoLink />
		</>
	)
}
