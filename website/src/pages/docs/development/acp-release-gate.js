import React from 'react';
import {Redirect, useLocation} from '@docusaurus/router';
import useBaseUrl from '@docusaurus/useBaseUrl';

export default function ReleaseQualificationRedirect() {
  const destination = useBaseUrl('/docs/development/release-qualification');
  const {search, hash} = useLocation();
  const target = `${destination}${search}${hash}`;

  return (
    <>
      <Redirect to={target} />
      <a href={target}>Continue to release qualification</a>
    </>
  );
}
