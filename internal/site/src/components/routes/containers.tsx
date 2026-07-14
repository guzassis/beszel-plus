import { useLingui } from "@lingui/react/macro"
import { memo, useEffect, useMemo } from "react"
import ContainersTable from "@/components/containers-table/containers-table"
import { ActiveAlerts } from "@/components/active-alerts"
import { FooterRepoLink } from "@/components/footer-repo-link"
import { pageTitle } from "@/lib/build-info"

export default memo(() => {
	const { t } = useLingui()

	useEffect(() => {
		document.title = pageTitle(t`All Containers`)
	}, [t])

	return useMemo(
		() => (
			<>
				<div className="grid gap-4">
					<ActiveAlerts />
					<ContainersTable />
				</div>
				<FooterRepoLink />
			</>
		),
		[]
	)
})
