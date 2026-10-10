import React from 'react';
import Link from '@docusaurus/Link';
import {demos} from '../data/demos';

export default function DemoVideo({videoId}) {
  const demo = demos.find(({id}) => id === videoId);

  if (!demo) {
    throw new Error(`Unknown demo video: ${videoId}`);
  }

  return (
    <figure className="demo-video">
      <iframe
        className="demo-video-frame"
        src={`https://www.youtube-nocookie.com/embed/${demo.id}`}
        title={demo.title}
        width="560"
        height="315"
        loading="lazy"
        referrerPolicy="strict-origin-when-cross-origin"
        allow="accelerometer; autoplay; clipboard-write; encrypted-media; gyroscope; picture-in-picture; web-share"
        allowFullScreen
      />
      <figcaption>
        <Link to={`https://www.youtube.com/watch?v=${demo.id}`}>
          {demo.title}
        </Link>
      </figcaption>
    </figure>
  );
}
